package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/spf13/cobra"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

func newServerProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ovn-server-proxy",
		Short: "A small tls proxy that will use HTTP_CONNECT to establish a connection if the HTTP_PROXY env var is set",
	}

	s := &ovnDBServer{}

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

type ovnDBServer struct {
	listenAddr string
	proxyAddr  string
	ovnDBAddr  string
}

func (s *ovnDBServer) validate() error {
	var errs []error
	if s.listenAddr == "" {
		errs = append(errs, errors.New("--listen-addr is mandatory"))
	}
	return utilerrors.NewAggregate(errs)
}

func (s *ovnDBServer) run(ctx context.Context) error {

	tcpAddr, err := net.ResolveTCPAddr("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to resolve tcp addr from %s: %w", s.listenAddr, err)
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("failed to listen tls on %s: %v", s.listenAddr, err)
	}

	fmt.Println("Starting to listen address: %s", s.listenAddr)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		conn, err := listener.AcceptTCP()
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

func (s *ovnDBServer) copyIngress(closer chan struct{}, src net.Conn) {
	r := bufio.NewReader(src)
	msg, err := r.ReadString('\n')
	if err != nil {
		fmt.Errorf("failed to read message: %s", err)
	}

	fmt.Println("message from local conn: %v", msg)
	closer <- struct{}{} // connection is closed, send signal to stop proxy
}
