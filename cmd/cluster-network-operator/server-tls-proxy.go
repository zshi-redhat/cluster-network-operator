package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/spf13/cobra"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

func newServerTLSProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ovn-server-tls-proxy",
		Short: "A small tls proxy that will use HTTP_CONNECT to establish a connection if the HTTP_PROXY env var is set",
	}

	s := &ovnDBServerTLS{}

	cmd.Flags().StringVar(&s.listenAddr, "listen-addr", "", "Address to listen on")
	cmd.Flags().StringVar(&s.proxyAddr, "proxy-addr", "", "Address of the proxy server")
	cmd.Flags().StringVar(&s.ovnDBAddr, "ovndb-addr", "", "Address of the ovn southbound database")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := s.validate(); err != nil {
			return err
		}
		return s.run(cmd.Context())
	}

	return cmd
}

type ovnDBServerTLS struct {
	listenAddr string
	proxyAddr  string
	ovnDBAddr  string
}

func (s *ovnDBServerTLS) validate() error {
	var errs []error
	if s.listenAddr == "" {
		errs = append(errs, errors.New("--listen-addr is mandatory"))
	}
	return utilerrors.NewAggregate(errs)
}

func (s *ovnDBServerTLS) run(ctx context.Context) error {

	cert, err := tls.LoadX509KeyPair("/ovn-cert/tls.crt", "/ovn-cert/tls.key")
	if err != nil {
		return fmt.Errorf("failed to load tls key pair: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	listener, err := tls.Listen("tcp", s.listenAddr, tlsConfig)
	if err != nil {
		return fmt.Errorf("failed to listen tls on %s: %v", s.listenAddr, err)
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

			closer := make(chan struct{}, 1)
			go s.copyIngress(closer, conn)
			<-closer

			fmt.Println("Connection completed")
		}()
	}
}

func (s *ovnDBServerTLS) copyIngress(closer chan struct{}, src net.Conn) {
	r := bufio.NewReader(src)
	msg, err := r.ReadString('\n')
	if err != nil {
		fmt.Errorf("failed to read message: %s", err)
	}

	fmt.Println("message from local conn: %v", msg)
	closer <- struct{}{} // connection is closed, send signal to stop proxy
}
