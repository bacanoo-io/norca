// Copyright IBM Corp. 2020, 2026
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Ported from Nomad Autoscaler v0.6.0 plugins/builtin/apm/prometheus/plugin/plugin.go.
// Modified: runs in-process inside the Nomad server; the go-plugin
// registration was dropped.

package prometheus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/bacanoo-io/norca/norca/helper/redact"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/plugins"
	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

const (
	// pluginName is the name of the plugin
	pluginName = "prometheus"

	// configKeyAddress is the accepted configuration key which holds the
	// address param.
	configKeyAddress = "address"

	// configKeyBasicAuthUser and configKeyBasicAuthPassword are the
	// configuration keys used to set the Prometheus client basic auth.
	configKeyBasicAuthUser     = "basic_auth_user"
	configKeyBasicAuthPassword = "basic_auth_password"

	// configKeyHeadersPrefix is the prefix used to indicate that a
	// configuration value should be set as an HTTP header.
	configKeyHeadersPrefix = "header_"

	// configKeyCACert is the path to the CA certificate the Prometheus client
	// should use.
	configKeyCACert = "ca_cert"

	// configKeySkipVerify indicates that the Prometheus client should not
	// verify TLS certificates.
	configKeySkipVerify = "skip_verify"
)

var (
	PluginID = plugins.PluginID{
		Name:       pluginName,
		PluginType: sdk.PluginTypeAPM,
	}

	pluginInfo = &plugins.PluginInfo{
		Name:       pluginName,
		PluginType: sdk.PluginTypeAPM,
	}
)

type APMPlugin struct {
	client api.Client
	config map[string]string
	logger hclog.Logger
}

func NewPrometheusPlugin(log hclog.Logger) plugins.APM {
	return &APMPlugin{
		logger: log,
	}
}

func (a *APMPlugin) SetConfig(config map[string]string) error {

	a.config = config

	// If the address is not set, or is empty within the config, any client
	// calls will fail. It seems logical to catch this here rather than just
	// let queries fail.
	addr, ok := a.config[configKeyAddress]
	if !ok || addr == "" {
		return fmt.Errorf("%q config value cannot be empty", configKeyAddress)
	}

	tlsConfig, err := generateTLSConfig(config)
	if err != nil {
		return fmt.Errorf("failed to parse TLS configuration: %v", err)
	}

	promCfg := api.Config{
		Address:      addr,
		RoundTripper: newPluginRoudTripper(a.config, tlsConfig),
	}

	// create Prometheus client
	client, err := api.NewClient(promCfg)
	if err != nil {
		return fmt.Errorf("failed to initialize Prometheus client: %v", redact.CallError(err, addr))
	}

	// store config and client in plugin instance
	a.client = client

	return nil
}

func (a *APMPlugin) PluginInfo() (*plugins.PluginInfo, error) {
	return pluginInfo, nil
}

func (a *APMPlugin) Query(q string, r sdk.TimeRange) (sdk.TimestampedMetrics, error) {
	m, err := a.QueryMultiple(q, r)
	if err != nil {
		return nil, err
	}

	switch len(m) {
	case 0:
		return sdk.TimestampedMetrics{}, nil
	case 1:
		return m[0], nil
	default:
		return nil, fmt.Errorf("query returned %d metric streams, only 1 is expected", len(m))
	}
}

func (a *APMPlugin) QueryMultiple(q string, r sdk.TimeRange) ([]sdk.TimestampedMetrics, error) {
	a.logger.Debug("querying Prometheus", "query", q, "range", r)

	v1api := v1.NewAPI(a.client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		result   model.Value
		warnings v1.Warnings
		err      error
	)

	if r.From.Equal(r.To) {
		result, warnings, err = v1api.Query(ctx, q, r.To)
	} else {
		promRange := v1.Range{Start: r.From, End: r.To, Step: queryStep(r)}
		result, warnings, err = v1api.QueryRange(ctx, q, promRange)
	}
	if err != nil {
		// The error names the URL of the query, which is the address of the
		// configuration with its credentials (basic authentication, an API
		// key in the query). It becomes the message of the evaluation in
		// the status of the policy, which the readers of the job see.
		return nil, fmt.Errorf("failed to query: %v", redact.CallError(err, a.config[configKeyAddress]))
	}

	// If Prometheus returned warnings, report these to the user.
	for _, w := range warnings {
		a.logger.Warn("prometheus query returned warning", "warning", w)
	}

	switch t := result.Type(); t {
	case model.ValScalar:
		resultScalar := result.(*model.Scalar)
		return parseScalar(resultScalar)
	case model.ValVector:
		resultVector := result.(model.Vector)
		return parseVector(resultVector)
	case model.ValMatrix:
		resultMatrix := result.(model.Matrix)
		return parseMatrix(resultMatrix)
	default:
		return nil, fmt.Errorf("result type (`%v`) is not supported", t)
	}
}

func generateTLSConfig(config map[string]string) (*tls.Config, error) {
	tlsConfig := tls.Config{}

	// Load the CA certificate if present.
	caCertPath := config[configKeyCACert]
	if caCertPath != "" {
		caCert, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load CA certificate %s: %v", caCertPath, err)
		}

		block, _ := pem.Decode(caCert)
		if block == nil {
			return nil, fmt.Errorf("failed to decode PEM file %s", caCertPath)
		}

		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caCertPool
	}

	skipVerify := config[configKeySkipVerify]
	if skipVerify != "" {
		skipVerifyBool, err := strconv.ParseBool(skipVerify)
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s value %s: %v", configKeySkipVerify, skipVerify, err)
		}

		tlsConfig.InsecureSkipVerify = skipVerifyBool
	}

	return &tlsConfig, nil
}

func parseScalar(s *model.Scalar) ([]sdk.TimestampedMetrics, error) {
	if s == nil {
		return nil, nil
	}

	tm, err := parseSample(*s)
	if err != nil {
		return nil, err
	}

	return []sdk.TimestampedMetrics{{tm}}, nil
}

// parseVector returns a stream for each series of an instant vector (Norca):
// the samples of several series were one stream, so Query took a query that
// returns several series for one, and a strategy used the value that sorted
// last (review finding ASP-5).
func parseVector(v model.Vector) ([]sdk.TimestampedMetrics, error) {
	result := make([]sdk.TimestampedMetrics, 0, len(v))
	for _, s := range v {
		tm, err := parseSample(*s)
		if err != nil {
			return nil, err
		}

		result = append(result, sdk.TimestampedMetrics{tm})
	}

	return result, nil
}

// maxQueryPoints is the largest number of points of a range query that
// Prometheus answers (Norca): it refuses a query of more than 11000 points
// a series.
const maxQueryPoints = 11000

// queryStep returns the step of a range query: one second, as before, while
// the window has fewer points than Prometheus answers, and a wider step, in
// whole seconds, for a longer window (Norca, review finding ASP-6: a window
// over about 3 hours failed with a step of 1 second).
func queryStep(r sdk.TimeRange) time.Duration {
	seconds := math.Ceil(r.To.Sub(r.From).Seconds() / (maxQueryPoints - 1))
	return time.Duration(max(seconds, 1)) * time.Second
}

func parseMatrix(m model.Matrix) ([]sdk.TimestampedMetrics, error) {
	// Cast matrix to a list of sample streams so we can iterate over it.
	ssList := []*model.SampleStream(m)
	result := make([]sdk.TimestampedMetrics, len(ssList))
	for i, ss := range ssList {
		var metrics sdk.TimestampedMetrics
		for _, sp := range ss.Values {
			tm, err := parseSample(sp)
			if err != nil {
				return nil, err
			}

			metrics = append(metrics, tm)
		}

		result[i] = metrics
	}

	return result, nil
}

func parseSample(s interface{}) (sdk.TimestampedMetric, error) {
	var ts model.Time
	var val model.SampleValue
	var result sdk.TimestampedMetric

	switch sType := s.(type) {
	case model.Scalar:
		val = sType.Value
		ts = sType.Timestamp
	case model.Sample:
		val = sType.Value
		ts = sType.Timestamp
	case model.SamplePair:
		val = sType.Value
		ts = sType.Timestamp
	default:
		return result, fmt.Errorf("invalid sample type %T", s)
	}

	valFloat := float64(val)
	// Check whether the sample value is an IEEE 754 not-a-number value.
	if math.IsNaN(valFloat) {
		return result, errors.New("query result value is not-a-number")
	}

	tsTime := time.Unix(int64(ts)/1e3, 0)

	return sdk.TimestampedMetric{
		Timestamp: tsTime,
		Value:     valFloat,
	}, nil
}
