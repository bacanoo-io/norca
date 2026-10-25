// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/nomad/controller/webhook"
)

// The "webhook" subcommand serves the admission webhooks of the
// LoadBalancer kind (see the webhook blocks of the operator pack's kind):
//
//   - /default (defaulting): fills in the class of the region
//     (-default-class) when the spec names none, so that the schema's
//     static default applies only where no webhook runs;
//   - /validate (validating): rejects classes no provider of the region
//     implements (-classes), which the schema can't know, and changes of
//     the class of an existing load balancer.

// webhookConfig configures the webhooks.
type webhookConfig struct {
	classes      []string
	defaultClass string
}

// newWebhookMux returns the handlers of the webhooks.
func newWebhookMux(cfg webhookConfig) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/default", webhook.Defaulting(func(_ context.Context, req *webhook.Request, spec map[string]any) ([]string, error) {
		if c, ok := spec["class"].(string); !ok || c == "" {
			spec["class"] = cfg.defaultClass
		}
		return nil, nil
	}))
	mux.Handle("/validate", webhook.Validating(func(_ context.Context, req *webhook.Request) ([]string, error) {
		if req.Operation == webhook.OperationDelete {
			return nil, nil
		}
		var spec Spec
		if err := req.Object.DecodeSpec(&spec); err != nil {
			return nil, fmt.Errorf("invalid spec: %w", err)
		}
		spec.Defaults()
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		if !slices.Contains(cfg.classes, spec.Class) {
			return nil, fmt.Errorf("class %q is not available in this region (available: %s)",
				spec.Class, strings.Join(cfg.classes, ", "))
		}
		if req.OldObject != nil {
			var old Spec
			if err := req.OldObject.DecodeSpec(&old); err == nil {
				old.Defaults()
				if old.Class != spec.Class {
					return nil, errors.New("the class of a load balancer can't be changed; delete and recreate it")
				}
			}
		}
		return nil, nil
	}))
	return mux
}

func runWebhook(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("webhook", flag.ContinueOnError)
	listen := fs.String("listen", ":8443", "address to listen on")
	certFile := fs.String("tls-cert", "", "PEM certificate of the webhook (its CA goes into the kind's ca_cert)")
	keyFile := fs.String("tls-key", "", "PEM key of the webhook")
	classes := fs.String("classes", DefaultClass, "comma-separated classes the providers of this region implement")
	defaultClass := fs.String("default-class", DefaultClass, "class filled in when the spec names none")
	logLevel := fs.String("log-level", "info", "log level")
	if err := fs.Parse(args); err != nil {
		return err
	}
	logger, err := newLogger(*logLevel)
	if err != nil {
		return err
	}
	if *certFile == "" || *keyFile == "" {
		return errors.New("-tls-cert and -tls-key are required: the servers call webhooks over https only")
	}
	cfg := webhookConfig{classes: strings.Split(*classes, ","), defaultClass: *defaultClass}
	logger.Info("webhook listening", "address", *listen, "classes", cfg.classes)
	return webhook.ListenAndServeTLS(ctx, *listen, *certFile, *keyFile, logRequests(logger, newWebhookMux(cfg)))
}

func logRequests(logger *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("webhook request", "path", r.URL.Path)
		h.ServeHTTP(w, r)
	})
}
