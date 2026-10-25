// Command ubixvault is the uBixVault server and CLI.
//
// Implemented so far: the `server` command, which runs the HTTP API over a
// file-backed, encrypted store. Initialization and unsealing are driven through
// the /v1/sys/* endpoints (see docs/DESIGN.md §4).
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/api"
	"github.com/cwolsen7905/ubixvault/internal/audit"
	"github.com/cwolsen7905/ubixvault/internal/client"
	"github.com/cwolsen7905/ubixvault/internal/cluster"
	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/ratelimit"
	"github.com/cwolsen7905/ubixvault/internal/seal"
	"github.com/cwolsen7905/ubixvault/internal/snapshot"
	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "server":
		if err := runServer(os.Args[2:]); err != nil {
			log.Fatalf("server: %v", err)
		}
	case "operator":
		if err := runOperator(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Printf("uBixVault %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Printf("uBixVault %s\n\n", version)
	fmt.Println("usage: ubixvault <command> [flags]")
	fmt.Println("\ncommands:")
	fmt.Println("  server                     run the HTTP API server")
	fmt.Println("  operator init              initialize a vault")
	fmt.Println("  operator unseal <key>      submit an unseal key")
	fmt.Println("  operator seal-status       show seal status")
	fmt.Println("  operator seal              re-seal (requires -token)")
	fmt.Println("  operator snapshot save <f> back up the encrypted store (requires -token)")
	fmt.Println("  operator snapshot restore -data <dir> <f>  restore a snapshot offline")
	fmt.Println("  operator rekey init        rotate the unseal shares (start; then update/status/cancel)")
	fmt.Println("  version                    print the version")
	fmt.Println("\nGlobal operator flags: -address (or $UBIXVAULT_ADDR), -token (or $UBIXVAULT_TOKEN)")
	fmt.Println("  TLS: -ca-cert <pem> (or $UBIXVAULT_CACERT), -tls-skip-verify (or $UBIXVAULT_TLS_SKIP_VERIFY)")
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8200", "address to listen on")
	storageType := fs.String("storage", "file", "storage backend: file or mysql")
	dataDir := fs.String("data", "./data", "directory for encrypted storage (-storage file)")
	storageDSN := fs.String("storage-mysql-dsn", os.Getenv("UBIXVAULT_STORAGE_DSN"),
		"MySQL/MariaDB DSN for -storage mysql (or $UBIXVAULT_STORAGE_DSN)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (enables HTTPS)")
	tlsKey := fs.String("tls-key", "", "TLS private key file")
	auditLog := fs.String("audit-log", "", "path to an audit log file (enables fail-closed audit logging)")
	autoUnsealKey := fs.String("auto-unseal-key", os.Getenv("UBIXVAULT_AUTO_UNSEAL_KEY"),
		"hex-encoded 32-byte key-encryption key; enables auto-unseal (or set $UBIXVAULT_AUTO_UNSEAL_KEY)")
	devNoTLS := fs.Bool("dev-no-tls", false, "allow serving plaintext HTTP on a non-loopback address (INSECURE)")
	sealTransitAddr := fs.String("seal-transit-address", os.Getenv("UBIXVAULT_SEAL_TRANSIT_ADDRESS"),
		"remote Transit-engine address for transit auto-unseal (or $UBIXVAULT_SEAL_TRANSIT_ADDRESS)")
	sealTransitToken := fs.String("seal-transit-token", os.Getenv("UBIXVAULT_SEAL_TRANSIT_TOKEN"),
		"token for the transit seal vault (or $UBIXVAULT_SEAL_TRANSIT_TOKEN)")
	sealTransitKey := fs.String("seal-transit-key", os.Getenv("UBIXVAULT_SEAL_TRANSIT_KEY"),
		"transit key name to wrap the master key with (or $UBIXVAULT_SEAL_TRANSIT_KEY)")
	sealTransitCACert := fs.String("seal-transit-ca-cert", "", "PEM CA bundle to trust for the transit seal vault")
	sealTransitSkipVerify := fs.Bool("seal-transit-tls-skip-verify", false,
		"skip TLS verification to the transit seal vault (INSECURE)")
	sealExternalCmd := fs.String("seal-external-command", os.Getenv("UBIXVAULT_SEAL_EXTERNAL_COMMAND"),
		"command that wraps/unwraps the master key via a KMS/HSM (`<cmd> wrap|unwrap` over stdin/stdout)")
	var sealExternalArgs stringSliceFlag
	fs.Var(&sealExternalArgs, "seal-external-arg", "extra argument passed before the wrap/unwrap mode (repeatable)")
	sealExternalTimeout := fs.Duration("seal-external-timeout", 30*time.Second,
		"timeout for each external seal command invocation")
	haEnabled := fs.Bool("ha", envTrue("UBIXVAULT_HA"),
		"active/standby HA over -storage mysql: replicas sharing the database elect one active (or $UBIXVAULT_HA)")
	haAdvertise := fs.String("ha-advertise-addr", os.Getenv("UBIXVAULT_HA_ADVERTISE_ADDR"),
		"URL other replicas reach this one on (default: scheme://$POD_IP-or-hostname:listen-port)")
	haClusterListen := fs.String("ha-cluster-listen", "",
		"address for the replica-to-replica cluster listener (mutual TLS; default: -listen's host, port 8201)")
	haClusterAddr := fs.String("ha-cluster-addr", os.Getenv("UBIXVAULT_HA_CLUSTER_ADDR"),
		"URL other replicas forward to (default: https://$POD_IP-or-hostname:cluster-port)")
	haLockTTL := fs.Duration("ha-lock-ttl", storage.DefaultLockTTL,
		"HA lock lease; an unplanned failover takes up to this long")
	haRetry := fs.Duration("ha-retry-interval", storage.DefaultLockRetryInterval,
		"how often a standby tries for the HA lock; a planned handoff takes up to this long")
	shutdownDelay := fs.Duration("shutdown-delay", 0,
		"on SIGTERM, keep serving this long (after an HA step-down) before closing listeners, so a load balancer stops routing here first")
	rateLimit := fs.Float64("rate-limit", 0, "per-client API requests/second (0 disables rate limiting)")
	rateBurst := fs.Float64("rate-limit-burst", 0, "rate-limit burst size; defaults to -rate-limit when unset")
	rateTrustFwd := fs.Bool("rate-limit-trust-forwarded", false,
		"key rate limits by X-Forwarded-For (enable only behind a trusted proxy)")
	logTLSAborts := fs.Bool("log-tls-handshake-aborts", false,
		"log every TLS handshake the client abandons (e.g. TCP health checks); by default they are counted and summarized every 5m")
	maxTokenTTL := fs.Duration("max-token-ttl", token.DefaultMaxTTL,
		"how far renewal may extend a token whose own TTL is shorter; a token deliberately issued for longer keeps its full TTL")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Refuse to serve secrets in the clear over a network: without TLS, only bind
	// to loopback unless explicitly overridden.
	tlsEnabled := *tlsCert != "" && *tlsKey != ""
	if err := checkTLSPolicy(*listen, tlsEnabled, *devNoTLS); err != nil {
		return err
	}

	phys, haBackend, err := openStorageBackend(*storageType, *dataDir, *storageDSN)
	if err != nil {
		return err
	}
	if *haEnabled && haBackend == nil {
		return fmt.Errorf("-ha requires -storage mysql (the %s backend cannot elect an active replica)", *storageType)
	}

	// At most one auto-unseal mode may be configured.
	sealModes := 0
	for _, set := range []bool{*autoUnsealKey != "", *sealTransitAddr != "", *sealExternalCmd != ""} {
		if set {
			sealModes++
		}
	}
	if sealModes > 1 {
		return fmt.Errorf("configure at most one auto-unseal mode: -auto-unseal-key, -seal-transit-*, or -seal-external-command")
	}

	var coreOpts []core.Option
	switch {
	case *sealExternalCmd != "":
		coreOpts = append(coreOpts, core.WithSeal(
			seal.NewExternal(*sealExternalCmd, sealExternalArgs, nil, *sealExternalTimeout)))
	case *sealTransitAddr != "":
		var caPEM []byte
		if *sealTransitCACert != "" {
			b, err := os.ReadFile(*sealTransitCACert)
			if err != nil {
				return fmt.Errorf("read seal-transit-ca-cert: %w", err)
			}
			caPEM = b
		}
		ts, err := seal.NewTransit(seal.TransitConfig{
			Address:    *sealTransitAddr,
			Token:      *sealTransitToken,
			Key:        *sealTransitKey,
			CACertPEM:  caPEM,
			SkipVerify: *sealTransitSkipVerify,
		})
		if err != nil {
			return err
		}
		coreOpts = append(coreOpts, core.WithSeal(ts))
	case *autoUnsealKey != "":
		kek, err := hex.DecodeString(*autoUnsealKey)
		if err != nil || len(kek) != 32 {
			return fmt.Errorf("auto-unseal-key must be 64 hex characters (32 bytes)")
		}
		coreOpts = append(coreOpts, core.WithAutoUnsealKey(kek))
	}
	if *haEnabled {
		adv := *haAdvertise
		if adv == "" {
			if adv, err = defaultAdvertise(*listen, tlsEnabled); err != nil {
				return err
			}
		}
		if *haClusterListen == "" {
			host, _, err := net.SplitHostPort(*listen)
			if err != nil {
				return fmt.Errorf("-ha: derive -ha-cluster-listen from -listen %q: %w", *listen, err)
			}
			*haClusterListen = net.JoinHostPort(host, "8201")
		}
		if *haClusterAddr == "" {
			// The cluster listener is always TLS, whatever the API does.
			if *haClusterAddr, err = defaultAdvertise(*haClusterListen, true); err != nil {
				return err
			}
		}
		holder, err := haHolderID()
		if err != nil {
			return err
		}
		coreOpts = append(coreOpts, core.WithHA(core.HAConfig{
			Backend:     haBackend,
			HolderID:    holder,
			Advertise:   adv,
			ClusterAddr: *haClusterAddr,
			Lock: storage.LockOptions{
				TTL:           *haLockTTL,
				RenewInterval: *haLockTTL / 3,
				RetryInterval: *haRetry,
			},
		}))
		log.Printf("HA enabled: replica %q, advertising %q, cluster %q", holder, adv, *haClusterAddr) //nolint:gosec // G706: operator-configured values, %q-quoted so they cannot forge log lines
	}
	c := core.New(phys, coreOpts...)
	c.Tokens().SetMaxTTL(*maxTokenTTL)

	opts := []api.Option{api.WithVersion(version)}

	// HA: the cluster identity comes from the barrier. The active replica
	// creates the CA; every replica drops its certificates when sealed; and a
	// standby forwards what it does not serve itself to the active replica.
	var certs *cluster.Certs
	var forwarder *cluster.Forwarder
	if c.HAEnabled() {
		certs = cluster.NewCerts(c.Barrier())
		c.OnActive(func() {
			if ensureClusterCA(certs) {
				return
			}
			// Keep trying for as long as this replica stays active: without the
			// CA, standbys cannot forward to it.
			go func() {
				for {
					time.Sleep(5 * time.Second)
					if !c.Active() || ensureClusterCA(certs) {
						return
					}
				}
			}()
		})
		c.OnSeal(certs.Reset)
		forwarder = cluster.NewForwarder(certs, func(ctx context.Context) (string, bool, error) {
			info, err := c.Leader(ctx)
			return info.ClusterAddr, info.IsSelf, err
		})
		opts = append(opts, api.WithForwarder(forwarder))
	}
	if *auditLog != "" {
		device, err := audit.NewFileDevice(*auditLog)
		if err != nil {
			return fmt.Errorf("open audit log: %w", err)
		}
		broker := audit.NewBroker(device)
		defer func() { _ = broker.Close() }()
		opts = append(opts, api.WithAudit(broker))
		log.Printf("audit logging to %s", *auditLog)
	}

	var limiter *ratelimit.Limiter
	if *rateLimit > 0 {
		burst := *rateBurst
		if burst <= 0 {
			burst = *rateLimit
		}
		limiter = ratelimit.New(*rateLimit, burst)
		opts = append(opts, api.WithRateLimit(limiter))
		if *rateTrustFwd {
			opts = append(opts, api.WithTrustForwardedFor())
		}
		log.Printf("rate limiting: %.4g req/s per client (burst %.4g)", *rateLimit, burst)
	}

	handler := api.NewHandler(c, opts...)
	if forwarder != nil {
		forwarder.ServeLocally(handler, c.Active)
	}

	// TCP health checks against a TLS listener abort the handshake, and net/http
	// logs each one. Hold those back (see handshakeAbortLog) unless asked not to.
	var serverLog *log.Logger
	var abortLog *handshakeAbortLog
	if !*logTLSAborts {
		abortLog = newHandshakeAbortLog(log.Default())
		serverLog = abortLog.Logger()
	}

	var clusterSrv *http.Server
	if certs != nil {
		ln, err := net.Listen("tcp", *haClusterListen)
		if err != nil {
			return fmt.Errorf("-ha: cluster listener: %w", err)
		}
		clusterSrv = &http.Server{
			Handler:           cluster.ServerHandler(handler, c.Active),
			TLSConfig:         certs.ServerTLS(),
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          serverLog,
		}
		go func() {
			if err := clusterSrv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("ERROR: HA cluster listener: %v", err)
			}
		}()
		log.Printf("HA cluster listener on %s (mutual TLS)", ln.Addr())
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          serverLog,
	}
	if tlsEnabled {
		// Request (but do not require) a client certificate, so the TLS
		// certificate auth method can use one when presented. The cert is NOT
		// verified by the TLS stack here — the auth method verifies it against the
		// CAs configured for each cert role.
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ClientAuth: tls.RequestClientCert,
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if abortLog != nil {
		go abortLog.Run(ctx, 5*time.Minute)
	}

	// Auto-unseal in the background, retrying until the seal and storage are
	// reachable, so a brief KMS or database outage at startup does not leave the
	// vault sealed until the next restart. The API is already up meanwhile:
	// livez answers, and health reports sealed (503) until this succeeds.
	if c.AutoUnsealEnabled() {
		go runAutoUnseal(ctx, c)
	}

	// With HA, take part in the election: become active when holding the lock,
	// and step down (releasing it) when sealed or shutting down.
	var haDone chan struct{}
	if c.HAEnabled() {
		haDone = make(chan struct{})
		go func() {
			defer close(haDone)
			_ = c.RunHA(ctx, log.Printf)
		}()
	}

	// Revoke expired dynamic-database leases in the background (active only).
	go handler.RunLeaseSweeper(ctx, time.Minute)

	// Delete expired tokens (active only), so logins do not accumulate forever.
	go handler.RunTokenSweeper(ctx, 10*time.Minute)

	// Periodically drop idle rate-limit buckets so memory stays bounded.
	if limiter != nil {
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					limiter.Sweep(10 * time.Minute)
				}
			}
		}()
	}

	storageDesc := *storageType
	if *storageType == "file" {
		storageDesc = "file:" + *dataDir
	}
	errCh := make(chan error, 1)
	go func() {
		if tlsEnabled {
			log.Printf("uBixVault %s listening on https://%s (storage: %s)", version, *listen, storageDesc)
			errCh <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			log.Printf("WARNING: serving plain HTTP without TLS — set -tls-cert/-tls-key for production")
			log.Printf("uBixVault %s listening on http://%s (storage: %s)", version, *listen, storageDesc)
			errCh <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Println("shutting down…")
		// Step down first, so a standby takes over while this replica drains.
		if haDone != nil {
			select {
			case <-haDone:
			case <-time.After(10 * time.Second):
				log.Printf("WARNING: HA step-down timed out; a standby takes over when the lease expires")
			}
		}
		// Keep answering (as a standby, forwarding) while load balancers notice
		// this replica is going: Kubernetes removes a terminating pod from its
		// Service asynchronously, and closing the listener at once would refuse
		// connections still being routed here.
		if *shutdownDelay > 0 {
			log.Printf("draining: still serving for %s", *shutdownDelay)
			time.Sleep(*shutdownDelay)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if clusterSrv != nil {
			_ = clusterSrv.Shutdown(shutdownCtx)
		}
		return srv.Shutdown(shutdownCtx)
	}
}

// ensureClusterCA creates the HA cluster CA if missing, logging a failure.
func ensureClusterCA(certs *cluster.Certs) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := certs.EnsureCA(ctx); err != nil {
		log.Printf("WARNING: HA cluster CA: %v (standbys cannot forward until it exists; retrying)", err)
		return false
	}
	return true
}

// defaultAdvertise derives this replica's HA advertise URL from the listen
// port and $POD_IP (set by the Helm chart), falling back to the hostname.
func defaultAdvertise(listen string, tlsEnabled bool) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("-ha: derive advertise address from -listen %q: %w", listen, err)
	}
	host := os.Getenv("POD_IP")
	if host == "" {
		if host, err = os.Hostname(); err != nil {
			return "", fmt.Errorf("-ha: set -ha-advertise-addr (no $POD_IP, hostname: %w)", err)
		}
	}
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

// haHolderID names this replica process in the HA lock: the hostname plus a
// random suffix, so a restarted pod with the same name is a new holder and
// cannot inherit a lease its previous process held.
func haHolderID() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		host = "ubixvault"
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("-ha: holder id: %w", err)
	}
	return host + "-" + hex.EncodeToString(b), nil
}

// Backoff bounds for the startup auto-unseal retry.
const (
	autoUnsealMinWait = time.Second
	autoUnsealMaxWait = time.Minute
)

// runAutoUnseal drives [core.Core.RunAutoUnseal] and logs its progress. An
// uninitialized vault is reported once rather than on every retry.
func runAutoUnseal(ctx context.Context, c *core.Core) {
	reportedUninit := false
	err := c.RunAutoUnseal(ctx, autoUnsealMinWait, autoUnsealMaxWait, func(err error, next time.Duration) {
		if errors.Is(err, core.ErrNotInitialized) {
			if !reportedUninit {
				log.Printf("auto-unseal configured; vault is not yet initialized")
				reportedUninit = true
			}
			return
		}
		log.Printf("WARNING: auto-unseal failed, retrying in %s: %v", next, err)
	})
	switch {
	case err == nil:
		log.Printf("auto-unsealed")
	case ctx.Err() != nil:
		// Shutting down.
	default:
		log.Printf("ERROR: auto-unseal cannot succeed with this configuration, staying sealed: %v", err)
	}
}

// checkTLSPolicy refuses to start a plaintext server on a non-loopback address
// unless explicitly overridden, so secrets are never served in the clear over a
// network by accident.
func checkTLSPolicy(listen string, tlsEnabled, devNoTLS bool) error {
	if tlsEnabled || devNoTLS || isLoopbackListen(listen) {
		return nil
	}
	return fmt.Errorf("refusing to serve plaintext HTTP on non-loopback address %q: "+
		"set -tls-cert/-tls-key, or pass -dev-no-tls to override (INSECURE)", listen)
}

// isLoopbackListen reports whether a listen address binds only the loopback
// interface. An empty/missing host (":8200", "0.0.0.0:8200") binds all
// interfaces and is not loopback. A non-IP hostname is treated conservatively as
// not loopback.
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen // no port present
	}
	switch host {
	case "":
		return false
	case "localhost":
		return true
	default:
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
}

// --- operator commands (a thin client over the HTTP API) ---

func runOperator(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("operator requires a subcommand: init, unseal, seal-status, seal")
	}
	switch args[0] {
	case "init":
		return operatorInit(args[1:])
	case "unseal":
		return operatorUnseal(args[1:])
	case "seal-status":
		return operatorSealStatus(args[1:])
	case "seal":
		return operatorSeal(args[1:])
	case "snapshot":
		return operatorSnapshot(args[1:])
	case "rekey":
		return operatorRekey(args[1:])
	default:
		return fmt.Errorf("unknown operator subcommand %q", args[0])
	}
}

func operatorRekey(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: operator rekey init | update | status | cancel")
	}
	switch args[0] {
	case "init":
		return operatorRekeyInit(args[1:])
	case "update":
		return operatorRekeyUpdate(args[1:])
	case "status":
		return operatorRekeyStatus(args[1:])
	case "cancel":
		return operatorRekeyCancel(args[1:])
	default:
		return fmt.Errorf("unknown rekey subcommand %q", args[0])
	}
}

func operatorRekeyInit(args []string) error {
	fs := flag.NewFlagSet("operator rekey init", flag.ExitOnError)
	cc := registerClientFlags(fs, false)
	shares := fs.Int("shares", 5, "number of new unseal key shares")
	threshold := fs.Int("threshold", 3, "new shares required to unseal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := cc.newClient()
	if err != nil {
		return err
	}
	st, err := c.RekeyInit(context.Background(), *shares, *threshold)
	if err != nil {
		return err
	}
	fmt.Printf("Rekey started.\n")
	fmt.Printf("Nonce:       %s\n", st.Nonce)
	fmt.Printf("New config:  %d shares, threshold %d\n", st.NewShares, st.NewThreshold)
	fmt.Printf("\nSubmit %d current unseal keys, one at a time:\n", st.Required)
	fmt.Printf("  ubixvault operator rekey update -nonce %s <key>\n", st.Nonce)
	return nil
}

func operatorRekeyUpdate(args []string) error {
	fs := flag.NewFlagSet("operator rekey update", flag.ExitOnError)
	cc := registerClientFlags(fs, false)
	nonce := fs.String("nonce", "", "rekey nonce from `operator rekey init`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := fs.Arg(0)
	if *nonce == "" || key == "" {
		return fmt.Errorf("usage: operator rekey update -nonce <nonce> <key>")
	}
	c, err := cc.newClient()
	if err != nil {
		return err
	}
	st, err := c.RekeyUpdate(context.Background(), *nonce, key)
	if err != nil {
		return err
	}
	if !st.Complete {
		fmt.Printf("Progress: %d/%d\n", st.Progress, st.Required)
		return nil
	}
	for i, k := range st.Keys {
		fmt.Printf("New Unseal Key %d: %s\n", i+1, k)
	}
	fmt.Printf("\nRekey complete — save these now, they are shown only once.\n")
	fmt.Printf("The old unseal keys no longer work. Unseal with any %d of the %d new keys.\n", st.NewThreshold, st.NewShares)
	return nil
}

func operatorRekeyStatus(args []string) error {
	fs := flag.NewFlagSet("operator rekey status", flag.ExitOnError)
	cc := registerClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := cc.newClient()
	if err != nil {
		return err
	}
	st, err := c.RekeyStatus(context.Background())
	if err != nil {
		return err
	}
	if !st.Started {
		fmt.Println("No rekey in progress.")
		return nil
	}
	fmt.Printf("Rekey in progress.\n")
	fmt.Printf("Nonce:       %s\n", st.Nonce)
	fmt.Printf("Progress:    %d/%d\n", st.Progress, st.Required)
	fmt.Printf("New config:  %d shares, threshold %d\n", st.NewShares, st.NewThreshold)
	return nil
}

func operatorRekeyCancel(args []string) error {
	fs := flag.NewFlagSet("operator rekey cancel", flag.ExitOnError)
	cc := registerClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := cc.newClient()
	if err != nil {
		return err
	}
	if err := c.RekeyCancel(context.Background()); err != nil {
		return err
	}
	fmt.Println("Rekey cancelled.")
	return nil
}

func operatorSnapshot(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: operator snapshot save <file> | restore [-storage file|mysql] <file>")
	}
	switch args[0] {
	case "save":
		return operatorSnapshotSave(args[1:])
	case "restore":
		return operatorSnapshotRestore(args[1:])
	default:
		return fmt.Errorf("unknown snapshot subcommand %q", args[0])
	}
}

func operatorSnapshotSave(args []string) error {
	fs := flag.NewFlagSet("operator snapshot save", flag.ExitOnError)
	cc := registerClientFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		return fmt.Errorf("usage: operator snapshot save [-address URL] [-token T] <file>")
	}
	f, err := os.Create(path) //nolint:gosec // G304: operator-provided output path
	if err != nil {
		return fmt.Errorf("create %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	c, err := cc.newClient()
	if err != nil {
		return err
	}
	if err := c.Snapshot(context.Background(), f); err != nil {
		return err
	}
	fmt.Printf("snapshot written to %s\n", path)
	return nil
}

// operatorSnapshotRestore restores a snapshot offline into a storage backend
// that the server is not running against. Start the server on that backend
// afterwards and unseal as usual. Restoring into a different backend than the
// source (e.g. file -> mysql) is how a vault migrates between backends.
func operatorSnapshotRestore(args []string) error {
	fs := flag.NewFlagSet("operator snapshot restore", flag.ExitOnError)
	storageType := fs.String("storage", "file", "storage backend to restore into: file or mysql")
	dataDir := fs.String("data", "", "data directory to restore into (-storage file; must be empty/new)")
	storageDSN := fs.String("storage-mysql-dsn", os.Getenv("UBIXVAULT_STORAGE_DSN"),
		"MySQL/MariaDB DSN to restore into (-storage mysql; or $UBIXVAULT_STORAGE_DSN)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := fs.Arg(0)
	if path == "" {
		return fmt.Errorf("usage: operator snapshot restore [-storage file|mysql] (-data <dir> | -storage-mysql-dsn <dsn>) <file>")
	}
	f, err := os.Open(path) //nolint:gosec // G304: operator-provided snapshot path
	if err != nil {
		return fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	backend, _, err := openStorageBackend(*storageType, *dataDir, *storageDSN)
	if err != nil {
		return err
	}
	if err := snapshot.Restore(context.Background(), backend, f); err != nil {
		return err
	}
	if *storageType == "mysql" {
		fmt.Printf("restored %s into the MySQL backend — start the server with -storage mysql and unseal\n", path)
	} else {
		fmt.Printf("restored %s into %s — start the server on this directory and unseal\n", path, *dataDir)
	}
	return nil
}

// openStorageBackend builds the physical storage backend selected by storageType
// ("file" uses dataDir; "mysql" uses dsn). Shared by the server and the offline
// snapshot-restore command.
func openStorageBackend(storageType, dataDir, dsn string) (storage.Backend, storage.HABackend, error) {
	switch storageType {
	case "file":
		if dataDir == "" {
			return nil, nil, fmt.Errorf("-storage file requires -data <dir>")
		}
		b, err := storage.NewFileBackend(dataDir)
		return b, nil, err
	case "mysql":
		if dsn == "" {
			return nil, nil, fmt.Errorf("-storage mysql requires -storage-mysql-dsn (or $UBIXVAULT_STORAGE_DSN)")
		}
		b, err := storage.NewMySQLBackend(dsn)
		if err != nil {
			return nil, nil, err
		}
		// Absorb brief MySQL outages (network blips, connection drops) with bounded
		// retry instead of failing every request; a sustained outage still fails
		// fast as storage.ErrUnavailable so readiness drops (see health) rather than
		// the process dying.
		// The HA lock comes from the unwrapped backend; its fencing applies to
		// writes that reach it through the retry wrapper too.
		return storage.NewRetryBackend(b), b, nil
	default:
		return nil, nil, fmt.Errorf("unknown -storage %q (want file or mysql)", storageType)
	}
}

func defaultAddr() string {
	if a := os.Getenv("UBIXVAULT_ADDR"); a != "" {
		return a
	}
	return "http://127.0.0.1:8200"
}

func envTrue(name string) bool {
	switch os.Getenv(name) {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

// stringSliceFlag collects a repeatable string flag into a slice, in order.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, " ") }

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// clientConfig holds the connection flags shared by the operator subcommands.
type clientConfig struct {
	addr       *string
	token      *string // nil when the command takes no -token
	caCert     *string
	skipVerify *bool
}

// registerClientFlags adds -address, -ca-cert and -tls-skip-verify to fs (and
// -token when withToken), each backed by the matching environment variable.
func registerClientFlags(fs *flag.FlagSet, withToken bool) *clientConfig {
	cc := &clientConfig{
		addr:       fs.String("address", defaultAddr(), "server address (or $UBIXVAULT_ADDR)"),
		caCert:     fs.String("ca-cert", os.Getenv("UBIXVAULT_CACERT"), "PEM CA bundle to trust for HTTPS (or $UBIXVAULT_CACERT)"),
		skipVerify: fs.Bool("tls-skip-verify", envTrue("UBIXVAULT_TLS_SKIP_VERIFY"), "skip TLS certificate verification, INSECURE (or $UBIXVAULT_TLS_SKIP_VERIFY)"),
	}
	if withToken {
		cc.token = fs.String("token", os.Getenv("UBIXVAULT_TOKEN"), "auth token (or $UBIXVAULT_TOKEN)")
	}
	return cc
}

// newClient builds a client from the parsed flags, applying TLS options.
func (cc *clientConfig) newClient() (*client.Client, error) {
	var opts []client.Option
	if *cc.skipVerify {
		opts = append(opts, client.WithTLSSkipVerify(true))
	}
	if *cc.caCert != "" {
		pem, err := os.ReadFile(*cc.caCert)
		if err != nil {
			return nil, fmt.Errorf("read ca-cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca-cert %q: no PEM certificates found", *cc.caCert)
		}
		opts = append(opts, client.WithRootCAs(pool))
	}
	token := ""
	if cc.token != nil {
		token = *cc.token
	}
	return client.New(*cc.addr, token, opts...), nil
}

func operatorInit(args []string) error {
	fs := flag.NewFlagSet("operator init", flag.ExitOnError)
	cc := registerClientFlags(fs, false)
	shares := fs.Int("shares", 5, "number of unseal key shares")
	threshold := fs.Int("threshold", 3, "shares required to unseal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := cc.newClient()
	if err != nil {
		return err
	}
	res, err := c.Init(context.Background(), *shares, *threshold)
	if err != nil {
		return err
	}
	for i, k := range res.Keys {
		fmt.Printf("Unseal Key %d: %s\n", i+1, k)
	}
	for i, k := range res.RecoveryKeys {
		fmt.Printf("Recovery Key %d: %s\n", i+1, k)
	}
	fmt.Printf("\nInitial Root Token: %s\n", res.RootToken)
	fmt.Printf("\nSave these now — they are shown only once.\n")
	if len(res.RecoveryKeys) > 0 {
		// Auto-unseal: the KEK unseals automatically; recovery keys exist only to
		// regenerate a lost root token.
		fmt.Printf("Auto-unseal is enabled — the server unseals itself. Keep the %d recovery\n", len(res.RecoveryKeys))
		fmt.Printf("keys safe: any %d of them can regenerate the root token if it is lost.\n", *threshold)
	} else {
		fmt.Printf("Unseal with any %d of the %d keys.\n", *threshold, *shares)
	}
	return nil
}

func operatorUnseal(args []string) error {
	fs := flag.NewFlagSet("operator unseal", flag.ExitOnError)
	cc := registerClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	key := fs.Arg(0)
	if key == "" {
		return fmt.Errorf("usage: operator unseal [-address URL] <key>")
	}
	c, err := cc.newClient()
	if err != nil {
		return err
	}
	st, err := c.Unseal(context.Background(), key)
	if err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func operatorSealStatus(args []string) error {
	fs := flag.NewFlagSet("operator seal-status", flag.ExitOnError)
	cc := registerClientFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := cc.newClient()
	if err != nil {
		return err
	}
	st, err := c.SealStatus(context.Background())
	if err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func operatorSeal(args []string) error {
	fs := flag.NewFlagSet("operator seal", flag.ExitOnError)
	cc := registerClientFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := cc.newClient()
	if err != nil {
		return err
	}
	if err := c.Seal(context.Background()); err != nil {
		return err
	}
	fmt.Println("Sealed.")
	return nil
}

func printStatus(st *client.SealStatus) {
	fmt.Printf("Initialized: %t\n", st.Initialized)
	fmt.Printf("Sealed:      %t\n", st.Sealed)
	if st.Initialized {
		fmt.Printf("Threshold:   %d\n", st.Threshold)
		fmt.Printf("Shares:      %d\n", st.Shares)
		fmt.Printf("Progress:    %d/%d\n", st.Progress, st.Threshold)
	}
}
