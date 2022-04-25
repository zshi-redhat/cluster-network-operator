package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

func newTLSRedirectProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ovn-tls-redirect-proxy",
		Short: "A small tcp proxy that will use HTTP_CONNECT to establish a connection if the HTTP_PROXY env var is set",
	}

	c := &ovnDBClientTLSRedirect{}

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

type ovnDBClientTLSRedirect struct {
	listenAddr string
	proxyAddr  string
	ovnDBAddr  string
	serverName string
}

func (c *ovnDBClientTLSRedirect) validate() error {
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

func (c *ovnDBClientTLSRedirect) run(ctx context.Context) error {

	cert, err := tls.LoadX509KeyPair("/ovn-cert/tls.crt", "/ovn-cert/tls.key")
	if err != nil {
		return fmt.Errorf("failed to load tls key pair: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	listener, err := tls.Listen("tcp", c.listenAddr, tlsConfig)
	if err != nil {
		return fmt.Errorf("failed to listen on tcp:%s: %w", c.listenAddr, err)
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

			backendConn, err := tls.Dial("tcp", c.proxyAddr, tlsConfig)
			if err != nil {
				fmt.Errorf("failed diaing backend proxyAddr %s: %v", c.proxyAddr, err)
				return
			}
			defer backendConn.Close()

			closer := make(chan struct{}, 2)
			go c.copy(closer, backendConn, conn)
			go c.copy(closer, conn, backendConn)
			<-closer

			fmt.Println("Connection completed")
		}()
	}
}

func (c *ovnDBClientTLSRedirect) copy(closer chan struct{}, dst io.Writer, src io.Reader) {
	r := bufio.NewReader(src)
	msg, err := r.ReadString('\n')
	if err != nil {
		fmt.Errorf("failed to read from connection: %s", err)
	}
	fmt.Println("message from conn: %v", msg)

	_, err = dst.Write([]byte(msg))
	if err != nil {
		fmt.Errorf("failed to write to connection: %s", err)
	}

	closer <- struct{}{} // connection is closed, send signal to stop proxy
}
