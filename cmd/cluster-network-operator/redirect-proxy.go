package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

type readOnlyConn struct {
	reader io.Reader
}

func (conn readOnlyConn) Read(p []byte) (int, error)         { return conn.reader.Read(p) }
func (conn readOnlyConn) Write(p []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (conn readOnlyConn) Close() error                       { return nil }
func (conn readOnlyConn) LocalAddr() net.Addr                { return nil }
func (conn readOnlyConn) RemoteAddr() net.Addr               { return nil }
func (conn readOnlyConn) SetDeadline(t time.Time) error      { return nil }
func (conn readOnlyConn) SetReadDeadline(t time.Time) error  { return nil }
func (conn readOnlyConn) SetWriteDeadline(t time.Time) error { return nil }

func newRedirectProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ovn-redirect-proxy",
		Short: "A small tcp proxy that will use HTTP_CONNECT to establish a connection if the HTTP_PROXY env var is set",
	}

	c := &ovnDBClientRedirect{}

	cmd.Flags().StringVar(&c.listenAddr, "listen-addr", "", "Address to listen on")
	cmd.Flags().StringVar(&c.proxyAddr, "proxy-addr", "", "Address of the proxy server")
	cmd.Flags().StringVar(&c.ovnDBAddr, "ovndb-addr", "", "Address of the ovn southbound database")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := c.validate(); err != nil {
			return err
		}
		return c.run(cmd.Context())
	}

	return cmd
}

type ovnDBClientRedirect struct {
	listenAddr string
	proxyAddr  string
	ovnDBAddr  string
}

func (c *ovnDBClientRedirect) validate() error {
	var errs []error
	if c.listenAddr == "" {
		errs = append(errs, errors.New("--listen-addr is mandatory"))
	}
	if c.proxyAddr == "" {
		errs = append(errs, errors.New("--proxy-addr is mandatory"))
	}
	if c.ovnDBAddr == "" {
		errs = append(errs, errors.New("--ovn-db is mandatory"))
	}
	return utilerrors.NewAggregate(errs)
}

func (c *ovnDBClientRedirect) run(ctx context.Context) error {

	// Listen TCP
	tcpAddr, err := net.ResolveTCPAddr("tcp", c.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve tcp addr from %s: %w", c.listenAddr, err)
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on tcp:%s: %w", c.listenAddr, err)
	}

	fmt.Println("Starting to listen address: %s", c.listenAddr)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// TCP conn
		conn, err := listener.AcceptTCP()
		if err != nil {
			fmt.Errorf("accepting connection failed: %v", err)
			continue
		}

		go func() {
			defer conn.Close()

			backendConn, err := net.Dial("tcp", c.proxyAddr)
			if err != nil {
				fmt.Errorf("failed diaing backend proxyAddr %s: %v", c.proxyAddr, err)
				return
			}
			defer backendConn.Close()

			req := &http.Request{
				Method:     "CONNECT",
				URL:        &url.URL{Host: strings.Split(c.ovnDBAddr, ":")[0]},
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
			}
			if err := req.Write(backendConn); err != nil {
				fmt.Errorf("failed to write connect request: %v", err)
				return
			}

			response, err := http.ReadResponse(bufio.NewReader(backendConn), req)
			if err != nil {
				fmt.Errorf("failed to read response to connect request: %v", err)
				return
			}
			if response.StatusCode != 200 {
				fmt.Errorf("got unexpected statuscode %d to CONNECT request, failed to establish a connection through http connect", response.StatusCode)
				return
			}

			closer := make(chan struct{}, 2)
			go c.copyEgress(closer, backendConn, conn)
			go c.copyIngress(closer, conn, backendConn)
			<-closer

			fmt.Println("Connection completed")
		}()
	}
}

func (c *ovnDBClientRedirect) encodeClientHello(s *tls.ClientHelloInfo) *bytes.Buffer {

	buf := &bytes.Buffer{}
	fmt.Println("client hello info: ", s)
	err := binary.Write(buf, binary.BigEndian, s)
	if err != nil {
		fmt.Errorf("failed to encode data", err)
	}
	return buf
}

func (c *ovnDBClientRedirect) peekClientHello(reader io.Reader) (*tls.ClientHelloInfo, io.Reader, error) {
	peekedBytes := new(bytes.Buffer)
	hello, err := c.readClientHello(io.TeeReader(reader, peekedBytes))
	if err != nil {
		return nil, nil, err
	}
	bufBytes := c.encodeClientHello(hello)
	return hello, io.MultiReader(bufBytes, reader), nil
}

func (c *ovnDBClientRedirect) readClientHello(reader io.Reader) (*tls.ClientHelloInfo, error) {
	var hello *tls.ClientHelloInfo

	err := tls.Server(readOnlyConn{reader: reader}, &tls.Config{
		GetConfigForClient: func(argHello *tls.ClientHelloInfo) (*tls.Config, error) {
			hello = new(tls.ClientHelloInfo)
			argHello.ServerName = strings.Split(c.ovnDBAddr, ":")[0]
			*hello = *argHello
			return nil, nil
		},
	}).Handshake()

	if hello == nil {
		return nil, err
	}
	return hello, nil
}

func (c *ovnDBClientRedirect) copyEgress(closer chan struct{}, dst io.Writer, src io.Reader) {
	clientHello, clientReader, err := c.peekClientHello(src)
	if err != nil {
		fmt.Errorf("failed to peek tls data", err)
	}
	fmt.Println("tls client hello server name", clientHello.ServerName)
	_, err = io.Copy(dst, clientReader)
	if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		fmt.Errorf("io.Copy failed: %s", err)
	}
	closer <- struct{}{} // connection is closed, send signal to stop proxy
}

func (c *ovnDBClientRedirect) copyIngress(closer chan struct{}, dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		fmt.Errorf("io.Copy failed: %s", err)
	}
	closer <- struct{}{} // connection is closed, send signal to stop proxy
}
