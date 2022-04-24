package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

func newTLSProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ovn-tls-proxy",
		Short: "A small tls proxy that will use HTTP_CONNECT to establish a connection if the HTTP_PROXY env var is set",
	}

	c := &ovnDBClientTLS{}

	cmd.Flags().StringVar(&c.listenAddr, "listen-addr", "", "Address to listen on")
	cmd.Flags().StringVar(&c.proxyAddr, "proxy-addr", "", "Address of the proxy server")
	cmd.Flags().StringVar(&c.ovnDBAddr, "ovndb-addr", "", "Address of the ovn southbound database")
	cmd.Flags().StringVar(&c.serverName, "server-name", "", "Server Name Indication")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := c.validate(); err != nil {
			return err
		}
		return c.run(cmd.Context())
	}

	return cmd
}

type ovnDBClientTLS struct {
	listenAddr string
	proxyAddr  string
	ovnDBAddr  string
	serverName string
}

func (c *ovnDBClientTLS) validate() error {
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
	if c.serverName == "" {
		errs = append(errs, errors.New("--server-name is mandatory"))
	}
	return utilerrors.NewAggregate(errs)
}

func (c *ovnDBClientTLS) run(ctx context.Context) error {

	cert, err := tls.LoadX509KeyPair("/ovn-cert/tls.crt", "/ovn-cert/tls.key")
	if err != nil {
		return fmt.Errorf("failed to load tls key pair: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	listener, err := tls.Listen("tcp", c.listenAddr, tlsConfig)
	if err != nil {
		return fmt.Errorf("failed to listen tls on %s: %v", c.listenAddr, err)
	}

	fmt.Println("Starting to listen address: %s", c.listenAddr)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		conn, err := listener.Accept()
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
				URL:        &url.URL{Host: c.ovnDBAddr},
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

func (c *ovnDBClientTLS) copyEgress(closer chan struct{}, dst io.Writer, src net.Conn) {
	r := bufio.NewReader(src)
	msg, err := r.ReadString('\n')
	if err != nil {
		fmt.Errorf("failed to read message: %s", err)
	}

	fmt.Println("message from local conn: %v", msg)

	if _, err := dst.Write([]byte(msg)); err != nil {
		fmt.Errorf("failed to write modified request: %s", err)
	}
	closer <- struct{}{} // connection is closed, send signal to stop proxy
}

func (c *ovnDBClientTLS) copyIngress(closer chan struct{}, dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		fmt.Errorf("io.Copy failed: %s", err)
	}
	closer <- struct{}{} // connection is closed, send signal to stop proxy
}
