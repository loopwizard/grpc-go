/*
 *
 * Copyright 2014, Google Inc.
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 *     * Redistributions of source code must retain the above copyright
 * notice, this list of conditions and the following disclaimer.
 *     * Redistributions in binary form must reproduce the above
 * copyright notice, this list of conditions and the following disclaimer
 * in the documentation and/or other materials provided with the
 * distribution.
 *     * Neither the name of Google Inc. nor the names of its
 * contributors may be used to endorse or promote products derived from
 * this software without specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
 * "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
 * LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
 * A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
 * OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 * SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 * LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
 * DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
 * THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 * (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 * OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 *
 */

// Package credentials implements various credentials supported by gRPC library,
// which encapsulate all the state needed by a client to authenticate with a
// server and make various assertions, e.g., about the client's identity, role,
// or whether it is authorized to make a particular call.
package credentials // import "google.golang.org/grpc/credentials"

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"
)

var (
	// alpnProtoStr are the specified application level protocols for gRPC.
	alpnProtoStr = []string{"h2-14", "h2-15", "h2-16"}
)

// Credentials defines the common interface all supported credentials must
// implement.
type Credentials interface {
	// GetRequestMetadata gets the current request metadata, refreshing
	// tokens if required. This should be called by the transport layer on
	// each request, and the data should be populated in headers or other
	// context. When supported by the underlying implementation, ctx can
	// be used for timeout and cancellation.
	// TODO(zhaoq): Define the set of the qualified keys instead of leaving
	// it as an arbitrary string.
	GetRequestMetadata(ctx context.Context) (map[string]string, error)
}

// TransportAuthenticator defines the common interface all supported transport
// authentication protocols (e.g., TLS, SSL) must implement.
type TransportAuthenticator interface {
	// Dial connects to the given network address using net.Dial and then
	// does the authentication handshake specified by the corresponding
	// authentication protocol.
	Dial(network, addr string) (net.Conn, error)
	// DialWithDialer connects to the given network address using
	// dialer.Dial does the authentication handshake specified by the
	// corresponding authentication protocol. Any timeout or deadline
	// given in the dialer apply to connection and handshake as a whole.
	DialWithDialer(dialer *net.Dialer, network, addr string) (net.Conn, error)
	// NewListener creates a listener which accepts connections with requested
	// authentication handshake.
	NewListener(lis net.Listener) net.Listener
	Credentials
}

// tlsCreds is the credentials required for authenticating a connection using TLS.
type tlsCreds struct {
	// TLS configuration
	config tls.Config
}

// GetRequestMetadata returns nil, nil since TLS credentials does not have
// metadata.
func (c *tlsCreds) GetRequestMetadata(ctx context.Context) (map[string]string, error) {
	return nil, nil
}

func (c *tlsCreds) DialWithDialer(dialer *net.Dialer, network, addr string) (_ net.Conn, err error) {
	if c.config.ServerName == "" {
		c.config.ServerName, _, err = net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("credentials: failed to parse server address %v", err)
		}
	}
	return tls.DialWithDialer(dialer, "tcp", addr, &c.config)
}

// Dial connects to addr and performs TLS handshake.
func (c *tlsCreds) Dial(network, addr string) (_ net.Conn, err error) {
	return c.DialWithDialer(new(net.Dialer), network, addr)
}

// NewListener creates a net.Listener using the information in tlsCreds.
func (c *tlsCreds) NewListener(lis net.Listener) net.Listener {
	return tls.NewListener(lis, &c.config)
}

// NewTLS uses c to construct a TransportAuthenticator based on TLS.
func NewTLS(c *tls.Config) TransportAuthenticator {
	tc := &tlsCreds{*c}
	tc.config.NextProtos = alpnProtoStr
	return tc
}

// NewClientTLSFromCert constructs a TLS from the input certificate for client.
func NewClientTLSFromCert(cp *x509.CertPool, serverName string) TransportAuthenticator {
	return NewTLS(&tls.Config{ServerName: serverName, RootCAs: cp})
}

// NewClientTLSFromFile constructs a TLS from the input certificate file for client.
func NewClientTLSFromFile(certFile, serverName string) (TransportAuthenticator, error) {
	b, err := ioutil.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("credentials: failed to append certificates")
	}
	return NewTLS(&tls.Config{ServerName: serverName, RootCAs: cp}), nil
}

// NewServerTLSFromCert constructs a TLS from the input certificate for server.
func NewServerTLSFromCert(cert *tls.Certificate) TransportAuthenticator {
	return NewTLS(&tls.Config{Certificates: []tls.Certificate{*cert}})
}

// NewServerTLSFromFile constructs a TLS from the input certificate file and key
// file for server.
func NewServerTLSFromFile(certFile, keyFile string) (TransportAuthenticator, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}}), nil
}

// TokenSource supplies credentials from an oauth2.TokenSource.
type TokenSource struct {
	oauth2.TokenSource
}

func (ts TokenSource) GetRequestMetadata(ctx context.Context) (map[string]string, error) {
	token, err := ts.Token()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"authorization": token.TokenType + " " + token.AccessToken,
	}, nil
}

// NewComputeEngine constructs the credentials that fetches access tokens from
// Google Compute Engine (GCE)'s metadata server. It is only valid to use this
// if your program is running on a GCE instance.
// TODO(dsymonds): Deprecate and remove this.
func NewComputeEngine() Credentials {
	return TokenSource{google.ComputeTokenSource("")}
}

// serviceAccount represents credentials via JWT signing key.
type serviceAccount struct {
	config *jwt.Config
}

func (s serviceAccount) GetRequestMetadata(ctx context.Context) (map[string]string, error) {
	token, err := s.config.TokenSource(ctx).Token()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"authorization": token.TokenType + " " + token.AccessToken,
	}, nil
}

// NewServiceAccountFromKey constructs the credentials using the JSON key slice
// from a Google Developers service account.
func NewServiceAccountFromKey(jsonKey []byte, scope ...string) (Credentials, error) {
	config, err := google.JWTConfigFromJSON(jsonKey, scope...)
	if err != nil {
		return nil, err
	}
	return serviceAccount{config: config}, nil
}

// NewServiceAccountFromFile constructs the credentials using the JSON key file
// of a Google Developers service account.
func NewServiceAccountFromFile(keyFile string, scope ...string) (Credentials, error) {
	jsonKey, err := ioutil.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("credentials: failed to read the service account key file: %v", err)
	}
	return NewServiceAccountFromKey(jsonKey, scope...)
}
