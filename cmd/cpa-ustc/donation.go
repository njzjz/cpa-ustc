package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cpa-ustc/internal/plugin"
)

// serveDonations deliberately has no flag accepting a raw management key: the
// key must stay out of shell history, process arguments and donor JavaScript.
func serveDonations(arguments []string) error {
	flags := flag.NewFlagSet("serve-donations", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8320", "捐献网关监听地址（建议仅本机）")
	backend := flags.String("cpa-url", "http://127.0.0.1:8317", "CPA 管理服务源地址，不含路径")
	origin := flags.String("origin", "", "公开捐献源地址，必须与插件 donation-origin 一致")
	keyEnvironment := flags.String("management-key-env", "CPA_MANAGEMENT_KEY", "仅服务器读取的 CPA 管理密钥环境变量名")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("不接受额外位置参数")
	}
	handler, err := plugin.NewDonationGateway(*backend, *origin, os.Getenv(*keyEnvironment))
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 70 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer listener.Close()
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		defer close(shutdownDone)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	fmt.Fprintln(os.Stderr, "捐献网关监听", listener.Addr(), "；请通过 HTTPS 反向代理对外提供服务")
	if err = server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns as soon as Shutdown closes the listener. Keep the process
	// alive until active confirmations finish or the shutdown deadline expires.
	<-shutdownDone
	return nil
}
