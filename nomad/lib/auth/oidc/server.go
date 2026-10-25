// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package oidc

import (
	"fmt"
	"net"
	"net/http"

	"github.com/hashicorp/cap/oidc"
	"github.com/hashicorp/nomad/api"
)

// CallbackServer is started with NewCallbackServer and creates an HTTP
// server for handling loopback OIDC auth redirects.
type CallbackServer struct {
	ln          net.Listener
	url         string
	clientNonce string
	errCh       chan error
	successCh   chan *api.ACLOIDCCompleteAuthRequest
}

// NewCallbackServer creates and starts a new local HTTP server for
// OIDC authentication to redirect to. This is used to capture the
// necessary information to complete the authentication.
func NewCallbackServer(addr string) (*CallbackServer, error) {
	// Generate our nonce
	nonce, err := oidc.NewID()
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	// Initialize our callback server
	srv := &CallbackServer{
		url:         fmt.Sprintf("http://%s/oidc/callback", addr),
		ln:          ln,
		clientNonce: nonce,
		errCh:       make(chan error, 5),
		successCh:   make(chan *api.ACLOIDCCompleteAuthRequest, 5),
	}

	// Register our HTTP route and start the server
	mux := http.NewServeMux()
	mux.Handle("/oidc/callback", srv)
	go func() {
		httpServer := &http.Server{Handler: mux}
		if err := httpServer.Serve(ln); err != nil {
			srv.errCh <- err
		}
	}()

	return srv, nil
}

// Close cleans up and shuts down the server. On close, errors may be
// sent to ErrorCh and should be ignored.
func (s *CallbackServer) Close() error { return s.ln.Close() }

// RedirectURI is the redirect URI that should be provided for the auth.
func (s *CallbackServer) RedirectURI() string { return s.url }

// Nonce returns a generated nonce that can be used for the request.
func (s *CallbackServer) Nonce() string { return s.clientNonce }

// ErrorCh returns a channel where any errors are sent. Errors may be
// sent after Close and should be disregarded.
func (s *CallbackServer) ErrorCh() <-chan error { return s.errCh }

// SuccessCh returns a channel that gets sent a partially completed
// request to complete the OIDC auth with the Nomad server.
func (s *CallbackServer) SuccessCh() <-chan *api.ACLOIDCCompleteAuthRequest { return s.successCh }

// ServeHTTP implements http.Handler and handles the callback request. This
// isn't usually used directly; use the server address instead.
func (s *CallbackServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()

	// Build our result
	result := &api.ACLOIDCCompleteAuthRequest{
		State:       q.Get("state"),
		ClientNonce: s.clientNonce,
		Code:        q.Get("code"),
	}

	// Send our result. We don't block here because the channel should be
	// buffered, and otherwise we're done.
	select {
	case s.successCh <- result:
	default:
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(serverSuccessHTMLResponse))
}

// serverSuccessHTMLResponse is the HTML response the OIDC callback server uses
// when the user has successfully logged in via the OIDC provider.
const serverSuccessHTMLResponse = `
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>OIDC Authentication Succeeded</title>
    <style>
      body {
        font-size: 14px;
        font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI",
          "Roboto", "Oxygen", "Ubuntu", "Cantarell", "Fira Sans", "Droid Sans",
          "Helvetica Neue", sans-serif;
      }
      hr {
        border-color: #fdfdfe;
        margin: 24px 0;
      }
      .container {
        display: flex;
        justify-content: center;
        align-items: center;
        height: 70vh;
      }
      svg.logo {
        display: block;
        margin: 0 0 20px;
      }
      .message {
        display: flex;
        min-width: 40vw;
        background: #f0f5ff;
        border: 1px solid #1563ff;
        margin-bottom: 12px;
        padding: 12px 16px 16px 12px;
        position: relative;
        border-radius: 2px;
        font-size: 14px;
        color: #0a2d74;
      }
      .message-content {
        margin-left: 4px;
      }
      .message #checkbox {
        fill: #1563ff;
      }
      .message .message-title {
        font-size: 16px;
        font-weight: 700;
        line-height: 1.25;
      }
      .message .message-body {
        border: 0;
        margin-top: 4px;
      }
      .message p {
        font-size: 12px;
        margin: 0;
        padding: 0;
      }
      a {
        display: block;
        margin: 8px 0;
        color: #1563ff;
        text-decoration: none;
        font-weight: 600;
      }
      a:hover {
        color: black;
      }
      a svg {
        fill: currentcolor;
      }
      .icon {
        align-items: center;
        display: inline-flex;
        justify-content: center;
        height: 21px;
        width: 21px;
        vertical-align: middle;
      }
      h1 {
        font-size: 17.5px;
        font-weight: 700;
        margin-bottom: 0;
      }
      h1 + p {
        margin: 8px 0 16px 0;
      }
    </style>
  </head>
  <body translate="no" >
    <div class="container">
      <div>
        <h1 class="brand">Norca</h1>
        <div class="message">
         <svg id="checkbox" aria-hidden="true" xmlns="http://www.w3.org/2000/svg" width="1em" height="1em" viewBox="0 0 16 16" fill="currentColor">
              <path fill-rule="evenodd" d="M8 16A8 8 0 108 0a8 8 0 000 16zm.93-9.412l-2.29.287-.082.38.45.083c.294.07.352.176.288.469l-.738 3.468c-.194.897.105 1.319.808 1.319.545 0 1.178-.252 1.465-.598l.088-.416c-.2.176-.492.246-.686.246-.275 0-.375-.193-.304-.533L8.93 6.588zM8 5.5a1 1 0 100-2 1 1 0 000 2z" clip-rule="evenodd"/>
          </svg>
          <div class="message-content">
            <div class="message-title">
              Signed in via your OIDC provider
            </div>
            <p class="message-body">
              You can now close this window and start using Norca.
            </p>
          </div>
        </div>
        <hr />
        <p>
          Run <code>norca -help</code> to list the available commands.
        </p>
      </div>
    </div>
  </body>
</html>
`
