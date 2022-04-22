package main

import (
	"bufio"
	"context"
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

func newProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ovn-proxy",
		Short: "A small tcp proxy that will use HTTP_CONNECT to establish a connection if the HTTP_PROXY env var is set",
	}

	s := &server{}

	cmd.Flags().StringVar(&s.listenAddr, "listen-addr", "", "Address to listen on")
	cmd.Flags().StringVar(&s.proxyAddr, "proxy-addr", "", "Address of the proxy server")
	cmd.Flags().StringVar(&s.ovnDBAddr, "ovndb-addr", "", "Address of the ovn southbound database")
	cmd.Flags().StringVar(&s.serverName, "server-name", "", "Server Name Indication")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := s.validate(); err != nil {
			return err
		}
		return s.run(cmd.Context())
	}

	return cmd
}

type server struct {
	listenAddr string
	proxyAddr  string
	ovnDBAddr  string
	serverName string
}

func (s *server) validate() error {
	var errs []error
	if s.listenAddr == "" {
		errs = append(errs, errors.New("--listen-addr is mandatory"))
	}
	if s.proxyAddr == "" {
		errs = append(errs, errors.New("--proxy-addr is mandatory"))
	}
	if s.ovnDBAddr == "" {
		errs = append(errs, errors.New("--ovn-db is mandatory"))
	}
	if s.serverName == "" {
		errs = append(errs, errors.New("--server-name is mandatory"))
	}
	return utilerrors.NewAggregate(errs)
}

func (s *server) run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on tcp:%s: %w", s.listenAddr, err)
	}
	fmt.Println("Starting to listen address: %s", s.listenAddr)

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

			backendConn, err := net.Dial("tcp", s.proxyAddr)
			if err != nil {
				fmt.Errorf("failed diaing backend proxyAddr %s: %v", s.proxyAddr, err)
				return
			}
			defer backendConn.Close()

			req := &http.Request{
				Method:     "CONNECT",
				URL:        &url.URL{Host: s.ovnDBAddr},
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
			go s.copyEgress(closer, backendConn, conn)
			go s.copyIngress(closer, conn, backendConn)
			<-closer

			fmt.Println("Connection completed")
		}()
	}
}

func (s *server) copyEgress(closer chan struct{}, dst io.Writer, src net.Conn) {
	request, err := http.ReadRequest(bufio.NewReader(src))
	if err != nil {
		fmt.Errorf("failed to read request from local conn: %s", err)
	}
	request.TLS.ServerName = s.serverName
	if err = request.Write(dst); err != nil {
		fmt.Errorf("failed to write modified request: %s", err)
	}
	closer <- struct{}{} // connection is closed, send signal to stop proxy
}

func (s *server) copyIngress(closer chan struct{}, dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		fmt.Errorf("io.Copy failed: %s", err)
	}
	closer <- struct{}{} // connection is closed, send signal to stop proxy
}
