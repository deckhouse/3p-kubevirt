package tls

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Pinned TLS", func() {
	var server, client *Identity

	BeforeEach(func() {
		var err error
		server, err = NewEphemeralIdentity("server")
		Expect(err).ToNot(HaveOccurred())
		client, err = NewEphemeralIdentity("client")
		Expect(err).ToNot(HaveOccurred())
	})

	handshake := func(serverCfg, clientCfg *tls.Config) (serverErr, clientErr error) {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
		Expect(err).ToNot(HaveOccurred())
		defer ln.Close()
		done := make(chan error, 1)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			done <- conn.(*tls.Conn).Handshake()
		}()
		conn, err := net.Dial("tcp", ln.Addr().String())
		Expect(err).ToNot(HaveOccurred())
		defer conn.Close()
		clientErr = tls.Client(conn, clientCfg).Handshake()
		return <-done, clientErr
	}

	DescribeTable("accepts pinned keys independently of certificate dates", func(notBefore, notAfter time.Time) {
		for _, identity := range []*Identity{server, client} {
			cert, err := x509.ParseCertificate(identity.cert.Certificate[0])
			Expect(err).ToNot(HaveOccurred())
			cert.NotBefore, cert.NotAfter = notBefore, notAfter
			der, err := x509.CreateCertificate(rand.Reader, cert, cert, cert.PublicKey, identity.cert.PrivateKey)
			Expect(err).ToNot(HaveOccurred())
			identity.cert.Certificate = [][]byte{der}
			identity.cert.Leaf = nil
			Expect(verifyPin(func() string { return "wrong" })([][]byte{der}, nil)).ToNot(Succeed())
		}
		serverErr, clientErr := handshake(server.ServerConfig(func() string { return client.Pin }), client.ClientConfig(func() string { return server.Pin }, "m1"))
		Expect(serverErr).ToNot(HaveOccurred())
		Expect(clientErr).ToNot(HaveOccurred())
	}, Entry("expired", time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)),
		Entry("not yet valid", time.Now().Add(24*time.Hour), time.Now().Add(48*time.Hour)))

	It("connects mutually pinned peers", func() {
		pin := func(p string) func() string { return func() string { return p } }
		serverErr, clientErr := handshake(server.ServerConfig(pin(client.Pin)), client.ClientConfig(pin(server.Pin), "m1"))
		Expect(serverErr).ToNot(HaveOccurred())
		Expect(clientErr).ToNot(HaveOccurred())
	})

	It("rejects a client with another key", func() {
		other, err := NewEphemeralIdentity("other")
		Expect(err).ToNot(HaveOccurred())
		pin := func(p string) func() string { return func() string { return p } }
		serverErr, _ := handshake(server.ServerConfig(pin(other.Pin)), client.ClientConfig(pin(server.Pin), "m1"))
		Expect(serverErr).To(HaveOccurred())
	})

	It("rejects a server with another key", func() {
		other, err := NewEphemeralIdentity("other")
		Expect(err).ToNot(HaveOccurred())
		pin := func(p string) func() string { return func() string { return p } }
		_, clientErr := handshake(server.ServerConfig(pin(client.Pin)), client.ClientConfig(pin(other.Pin), "m1"))
		Expect(clientErr).To(HaveOccurred())
	})

	It("rejects everyone until the pin of the peer is known", func() {
		pin := func(p string) func() string { return func() string { return p } }
		serverErr, _ := handshake(server.ServerConfig(pin("")), client.ClientConfig(pin(server.Pin), "m1"))
		Expect(serverErr).To(HaveOccurred())
	})
})
