// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/hashicorp/go-msgpack/codec"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
)

type Event struct {
	srv *Server
}

func NewEventEndpoint(srv *Server) *Event {
	return &Event{srv: srv}
}

func (e *Event) register() {
	e.srv.streamingRpcs.Register("Event.Stream", e.stream)
}

func (e *Event) stream(conn io.ReadWriteCloser) {
	defer conn.Close()

	var args structs.EventStreamRequest
	decoder := codec.NewDecoder(conn, structs.MsgpackHandle)
	encoder := codec.NewEncoder(conn, structs.MsgpackHandle)

	if err := decoder.Decode(&args); err != nil {
		handleJsonResultError(err, pointer.Of(int64(500)), encoder)
		return
	}

	authErr := e.srv.Authenticate(nil, &args)

	// forward to appropriate region
	if args.Region != e.srv.config.Region {
		err := e.forwardStreamingRPC(args.Region, "Event.Stream", args, conn)
		if err != nil {
			handleJsonResultError(err, pointer.Of(int64(500)), encoder)
		}
		return
	}

	e.srv.MeasureRPCRate("event", structs.RateMetricRead, &args)
	if authErr != nil {
		handleJsonResultError(structs.ErrPermissionDenied, pointer.Of(int64(http.StatusForbidden)), encoder)
		return
	}

	// Generate the subscription request
	subReq := &stream.SubscribeRequest{
		Token:     args.AuthToken,
		Topics:    args.Topics,
		Index:     uint64(args.Index),
		Namespace: args.Namespace,

		StartExactlyAtIndex: args.StartExactlyAtIndex,
	}

	// Get the servers broker and subscribe
	publisher, err := e.srv.State().EventBroker()
	if err != nil {
		handleJsonResultError(err, pointer.Of(int64(500)), encoder)
		return
	}

	// start subscription to publisher
	var subscription *stream.Subscription
	var subErr error

	// Track whether the ACL token being used has an expiry time.
	var expiryTime *time.Time

	// A workload identity is valid only while its allocation is not
	// terminal (see Server.VerifyClaim).
	var identityAllocID string

	// Check required ACL permissions for requested Topics
	if e.srv.config.ACLEnabled {
		if claims := args.GetIdentity().GetClaims(); claims != nil {
			identityAllocID = claims.AllocationID
		}
		subReq.ResolveACL = e.resolveSubscriptionACL
		subscription, expiryTime, subErr = publisher.SubscribeWithACLCheck(subReq)
	} else {
		subscription, subErr = publisher.Subscribe(subReq)
	}
	if subErr != nil {
		handleJsonResultError(subErr, pointer.Of(subscribeErrorCode(subErr)), encoder)
		return
	}
	defer subscription.Unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// goroutine to detect remote side closing
	go func() {
		io.Copy(io.Discard, conn)
		cancel()
	}()

	jsonStream := stream.NewJsonStream(ctx, 30*time.Second)
	errCh := make(chan error)
	go func() {
		defer cancel()
		for {
			events, err := subscription.Next(ctx)
			if err != nil {
				select {
				case errCh <- err:
				case <-ctx.Done():
				}
				return
			}

			// Ensure the token being used is not expired before we any events
			// to subscribers.
			if expiryTime != nil && expiryTime.Before(time.Now().UTC()) {
				select {
				case errCh <- structs.ErrTokenExpired:
				case <-ctx.Done():
				}
				return
			}
			if identityAllocID != "" && !e.allocAlive(identityAllocID) {
				select {
				case errCh <- structs.ErrTokenExpired:
				case <-ctx.Done():
				}
				return
			}

			// Continue if there are no events
			if len(events.Events) == 0 {
				continue
			}

			if err := jsonStream.Send(events); err != nil {
				select {
				case errCh <- err:
				case <-ctx.Done():
				}
				return
			}
		}
	}()

	var streamErr error
OUTER:
	for {
		select {
		case streamErr = <-errCh:
			break OUTER
		case <-ctx.Done():
			break OUTER
		case eventJSON, ok := <-jsonStream.OutCh():
			// check if ndjson may have been closed when an error occurred,
			// check once more for an error.
			if !ok {
				select {
				case streamErr = <-errCh:
					// There was a pending error
				default:
				}
				break OUTER
			}

			var resp structs.EventStreamWrapper
			resp.Event = eventJSON

			if err := encoder.Encode(resp); err != nil {
				streamErr = err
				break OUTER
			}
			encoder.Reset(conn)
		}

	}

	if streamErr != nil {
		handleJsonResultError(streamErr, pointer.Of(int64(500)), encoder)
		return
	}

}

// resolveSubscriptionACL resolves the ACL object and expiry time of an event
// stream token. Like other RPCs it accepts ACL tokens and workload
// identities, whose ACL comes from the policies bound to their job.
func (e *Event) resolveSubscriptionACL(token string) (*acl.ACL, *time.Time, error) {
	args := &structs.GenericRequest{QueryOptions: structs.QueryOptions{AuthToken: token}}
	if err := e.srv.Authenticate(nil, args); err != nil {
		return nil, nil, err
	}
	aclObj, err := e.srv.ResolveACL(args)
	if err != nil {
		return nil, nil, err
	}
	if aclObj == nil {
		// The secret identifies neither an ACL token nor a workload, for
		// example it is a node secret ID.
		return nil, nil, structs.ErrPermissionDenied
	}

	identity := args.GetIdentity()
	if aclToken := identity.GetACLToken(); aclToken != nil {
		return aclObj, aclToken.ExpirationTime, nil
	}
	if claims := identity.GetClaims(); claims != nil && claims.ExpiresAt != nil {
		return aclObj, pointer.Of(claims.ExpiresAt.Time), nil
	}
	return aclObj, nil, nil
}

// allocAlive reports whether the allocation of a workload identity exists and
// is not terminal.
func (e *Event) allocAlive(allocID string) bool {
	alloc, err := e.srv.State().AllocByID(nil, allocID)
	return err == nil && alloc != nil && !alloc.ClientTerminalStatus()
}

// subscribeErrorCode returns the HTTP status code for an error returned when
// subscribing to the event broker.
func subscribeErrorCode(err error) int64 {
	switch {
	case errors.Is(err, stream.ErrIndexNotAvailable):
		// The client has to resynchronize (list, then watch again).
		return http.StatusGone
	case errors.Is(err, stream.ErrExactIndexRequired):
		return http.StatusBadRequest
	case errors.Is(err, structs.ErrPermissionDenied),
		errors.Is(err, structs.ErrTokenNotFound),
		errors.Is(err, structs.ErrTokenExpired),
		errors.Is(err, structs.ErrTokenInvalid):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

func (e *Event) forwardStreamingRPC(region string, method string, args interface{}, in io.ReadWriteCloser) error {
	server, err := e.srv.findRegionServer(region)
	if err != nil {
		return err
	}

	return e.forwardStreamingRPCToServer(server, method, args, in)
}

func (e *Event) forwardStreamingRPCToServer(server *serverParts, method string, args interface{}, in io.ReadWriteCloser) error {
	srvConn, err := e.srv.streamingRpc(server, method)
	if err != nil {
		return err
	}
	defer srvConn.Close()

	outEncoder := codec.NewEncoder(srvConn, structs.MsgpackHandle)
	if err := outEncoder.Encode(args); err != nil {
		return err
	}

	structs.Bridge(in, srvConn)
	return nil
}

// handleJsonResultError is a helper for sending an error with a potential
// error code. The transmission of the error is ignored if the error has been
// generated by the closing of the underlying transport.
func handleJsonResultError(err error, code *int64, encoder *codec.Encoder) {
	// Nothing to do as the conn is closed
	if err == io.EOF {
		return
	}

	encoder.Encode(&structs.EventStreamWrapper{
		Error: structs.NewRpcError(err, code),
	})
}
