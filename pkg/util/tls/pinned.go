package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// A decentralized migration crosses a cluster boundary, where no CA is shared. Instead
// each side generates a key for the one migration and the peers verify each other by the
// SHA-256 of the public key (the pin), exchanged out of band by the management layer.
// The key never leaves the process and dies with the migration.

const (
	PinPrefix = "sha256:"

	ephemeralIdentityTTL = 24 * time.Hour
	clockSkewAllowance   = 5 * time.Minute
)

type Identity struct {
	cert tls.Certificate
	Pin  string
}

func NewEphemeralIdentity(commonName string) (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-clockSkewAllowance),
		NotAfter:              now.Add(ephemeralIdentityTTL),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Identity{
		cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		Pin:  Pin(leaf),
	}, nil
}

func Pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return PinPrefix + base64.StdEncoding.EncodeToString(sum[:])
}

// ServerConfig accepts only a client whose key matches peerPin, read at handshake time
// because the peer publishes its pin after the listener is up.
func (id *Identity) ServerConfig(peerPin func() string) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{id.cert},
		ClientAuth:            tls.RequireAnyClientCert,
		MinVersion:            tls.VersionTLS12,
		VerifyPeerCertificate: verifyPin(peerPin),
	}
}

// ClientConfig trusts only the server whose key matches peerPin; serverName is sent as
// SNI so a server hosting several migrations can pick the matching key.
func (id *Identity) ClientConfig(peerPin func() string, serverName string) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{id.cert},
		ServerName:   serverName,
		// The pin check in VerifyPeerCertificate is the verification, no CA is involved.
		InsecureSkipVerify:    true,
		MinVersion:            tls.VersionTLS12,
		VerifyPeerCertificate: verifyPin(peerPin),
	}
}

func verifyPin(peerPin func() string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("the peer presented no certificate")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parse the peer certificate: %w", err)
		}
		expected := peerPin()
		if expected == "" {
			return errors.New("the pin of the peer is not known yet")
		}
		if subtle.ConstantTimeCompare([]byte(Pin(cert)), []byte(expected)) != 1 {
			return errors.New("the peer key does not match the pinned one")
		}
		return nil
	}
}
