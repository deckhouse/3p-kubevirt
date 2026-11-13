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
			Expect(verifyPin(func() string { return "wrong" })(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}})).ToNot(Succeed())
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

	DescribeTable("checks pins when a TLS session resumes", func(version uint16, scenario string) {
		otherServer, err := NewEphemeralIdentity("other-server")
		Expect(err).NotTo(HaveOccurred())
		otherClient, err := NewEphemeralIdentity("other-client")
		Expect(err).NotTo(HaveOccurred())
		expectedClient, expectedServer := client.Pin, server.Pin
		serverConfig := &tls.Config{
			MinVersion: version, MaxVersion: version,
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				if hello.ServerName == "m2" {
					return otherServer.ServerConfig(func() string { return otherClient.Pin }), nil
				}
				return server.ServerConfig(func() string { return expectedClient }), nil
			},
		}
		clientConfig := client.ClientConfig(func() string { return expectedServer }, "m1")
		clientConfig.MinVersion, clientConfig.MaxVersion = version, version
		// Reuse the ticket across names to model a client trying another migration's SNI.
		cache := tls.NewLRUClientSessionCache(1)
		clientConfig.ClientSessionCache = &migrationSessionCache{ClientSessionCache: cache}

		exchange := func() (tls.ConnectionState, error, error) {
			ln, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
			Expect(err).NotTo(HaveOccurred())
			defer ln.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				err = conn.(*tls.Conn).Handshake()
				if err == nil {
					conn.Write([]byte{1})
					conn.Read(make([]byte, 1))
				}
				done <- err
			}()
			conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			tlsConn := tls.Client(conn, clientConfig)
			// Reading application data also processes the TLS 1.3 session ticket.
			_, clientErr := tlsConn.Read(make([]byte, 1))
			if clientErr == nil {
				_, clientErr = tlsConn.Write([]byte{1})
			}
			tlsConn.Close()
			return tlsConn.ConnectionState(), <-done, clientErr
		}
		_, serverErr, clientErr := exchange()
		Expect(serverErr).NotTo(HaveOccurred(), "the initial connection must succeed (client error: %v)", clientErr)
		Expect(clientErr).NotTo(HaveOccurred())
		state, serverErr, clientErr := exchange()
		Expect(serverErr).NotTo(HaveOccurred(), "resumption client error: %v", clientErr)
		Expect(clientErr).NotTo(HaveOccurred())
		Expect(state.DidResume).To(BeTrue(), "the regression must exercise session resumption")

		switch scenario {
		case "client pin changed":
			expectedClient = otherClient.Pin
		case "server pin changed":
			expectedServer = otherServer.Pin
		case "migration changed":
			clientConfig.ServerName = "m2"
		}
		_, serverErr, clientErr = exchange()
		if scenario == "server pin changed" {
			Expect(clientErr).To(MatchError(ContainSubstring("the peer key does not match the pinned one")), "the client must check the current server pin on resumption")
		} else {
			Expect(serverErr).To(MatchError(ContainSubstring("the peer key does not match the pinned one")), "the server must check the current migration's client pin on resumption")
		}
	},
		Entry("TLS 1.2 client pin changed", uint16(tls.VersionTLS12), "client pin changed"),
		Entry("TLS 1.3 client pin changed", uint16(tls.VersionTLS13), "client pin changed"),
		Entry("TLS 1.2 server pin changed", uint16(tls.VersionTLS12), "server pin changed"),
		Entry("TLS 1.3 server pin changed", uint16(tls.VersionTLS13), "server pin changed"),
		Entry("TLS 1.2 migration changed", uint16(tls.VersionTLS12), "migration changed"),
		Entry("TLS 1.3 migration changed", uint16(tls.VersionTLS13), "migration changed"),
	)
})

// migrationSessionCache deliberately offers the same ticket for different SNI names.
type migrationSessionCache struct {
	tls.ClientSessionCache
}

func (c *migrationSessionCache) Get(string) (*tls.ClientSessionState, bool) {
	return c.ClientSessionCache.Get("shared")
}

func (c *migrationSessionCache) Put(_ string, state *tls.ClientSessionState) {
	c.ClientSessionCache.Put("shared", state)
}
