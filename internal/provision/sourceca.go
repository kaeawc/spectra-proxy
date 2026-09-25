package provision

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
)

// loadSourceCAs strictly parses a PEM bundle: every block must be a valid
// certificate and nothing else may appear, so a truncated or wrong file fails
// instead of silently trusting fewer roots than intended.
func loadSourceCAs(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read source CA file: %w", err)
	}
	pool := x509.NewCertPool()
	count := 0
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("source CA file %s contains a %q block; only certificates are allowed", path, block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("source CA file %s: parse certificate: %w", path, err)
		}
		pool.AddCert(cert)
		count++
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("source CA file %s contains non-PEM data", path)
	}
	if count == 0 {
		return nil, fmt.Errorf("source CA file %s contains no certificates", path)
	}
	return pool, nil
}

// withSourceRoots returns a copy of base whose TLS verification trusts only pool.
func withSourceRoots(base *http.Client, pool *x509.CertPool) *http.Client {
	client := &http.Client{}
	if base != nil {
		*client = *base
	}
	var transport *http.Transport
	if t, ok := client.Transport.(*http.Transport); ok {
		transport = t.Clone()
	} else {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport.TLSClientConfig.RootCAs = pool
	client.Transport = transport
	return client
}
