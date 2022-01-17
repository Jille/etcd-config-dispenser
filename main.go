package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	clientconfig "github.com/Jille/etcd-client-from-env"
	"github.com/spf13/pflag"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	prefix       = pflag.StringP("prefix", "p", "", "Prefix of etcd files to fetch")
	target       = pflag.StringP("target", "t", "", "Target directory to install etcd content to")
	reloadSignal = pflag.StringP("signal", "s", "", "Signal to send when config files are changed")
)

var (
	c         *clientv3.Client
	sigCh     = make(chan os.Signal, 1000)
	reloadSig os.Signal
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	ctx := context.Background()
	pflag.Parse()
	*prefix = strings.TrimSuffix(*prefix, "/") + "/"
	switch *reloadSignal {
	case "SIGINT":
		reloadSig = syscall.SIGINT
	case "SIGALRM":
		reloadSig = syscall.SIGALRM
	case "SIGCONT":
		reloadSig = syscall.SIGCONT
	case "SIGHUP":
		reloadSig = syscall.SIGHUP
	case "SIGWINCH":
		reloadSig = syscall.SIGWINCH
	case "SIGUSR1":
		reloadSig = syscall.SIGUSR1
	case "SIGUSR2":
		reloadSig = syscall.SIGUSR2
	case "":
		// Don't send a signal to reload.
	default:
		log.Fatalf("Unknown reload signal %q", *reloadSignal)
	}

	cc, err := clientconfig.Get()
	if err != nil {
		log.Fatalf("Failed to parse environment settings: %v", err)
	}
	log.Printf("Connecting to etcd...")
	c, err = clientv3.New(cc)
	if err != nil {
		log.Fatalf("Failed to connect to etcd: %v", err)
	}
	defer c.Close()
	log.Printf("Connected.")
	go c.Sync(ctx)

	initialSync := make(chan struct{})
	go syncLoopLoop(ctx, initialSync)

	<-initialSync

	signal.Notify(sigCh, syscall.SIGABRT, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGWINCH)

	cmd := exec.Command(pflag.Arg(0), pflag.Args()[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start %s: %v", pflag.Arg(0), err)
	}
	go func() {
		for sig := range sigCh {
			log.Printf("Forwarding signal: %v", sig)
			if err := cmd.Process.Signal(sig); err != nil {
				log.Fatalf("Failed to deliver signal %s to %s: %v", sig, pflag.Arg(0), err)
			}
		}
	}()
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				if ws.Signaled() {
					// Try to die by the same signal that killed our child to mimic the exit code.
					log.Printf("%s was killed by: %s", pflag.Arg(0), ws.Signal())
					signal.Reset(ws.Signal())
					syscall.Kill(0, ws.Signal())
				}
			}
			os.Exit(ee.ExitCode())
		}
		log.Fatalf("%s died: %v", pflag.Arg(0), err)
	}
}

func syncLoopLoop(ctx context.Context, initialSync chan struct{}) {
	for {
		if err := syncLoop(ctx, initialSync); err != nil {
			select {
			case <-initialSync:
				log.Print(err)
			default:
				log.Fatal(err)
			}
		}
		time.Sleep(10 * time.Second)
	}
}

func syncLoop(ctx context.Context, initialSync chan struct{}) error {
	ctxGet, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := c.Get(ctxGet, *prefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("Failed to retrieve initial keys: %v", err)
	}
	for _, kv := range resp.Kvs {
		if err := ioutil.WriteFile(pathForKey(kv.Key), kv.Value, 0600); err != nil {
			return fmt.Errorf("Failed to write to %q: %v", pathForKey(kv.Key), err)
		}
	}
	cancel()
	select {
	case <-initialSync:
	default:
		close(initialSync)
	}
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	wc := c.Watch(ctx, *prefix, clientv3.WithPrefix(), clientv3.WithRev(resp.Header.Revision))
	for wr := range wc {
		if err := wr.Err(); err != nil {
			return err
		}
		if wr.Header.Revision == resp.Header.Revision {
			// When we subscribe on revision X, we get an immediate notification if X was a revision that changed our watched files.
			// We just fetched that, so we should ignore this to avoid sending an immediate reload signal.
			continue
		}
		for _, e := range wr.Events {
			switch e.Type {
			case clientv3.EventTypePut:
				if err := ioutil.WriteFile(pathForKey(e.Kv.Key), e.Kv.Value, 0600); err != nil {
					return fmt.Errorf("Failed to write to %q: %v", pathForKey(e.Kv.Key), err)
				}
			case clientv3.EventTypeDelete:
				if err := os.Remove(pathForKey(e.Kv.Key)); err != nil {
					return fmt.Errorf("Failed to remove %q: %v", pathForKey(e.Kv.Key), err)
				}
			}
		}
		if len(wr.Events) > 0 {
			// TODO: Only reload if there were changes?
			if reloadSig != nil {
				sigCh <- reloadSig
			}
		}
	}
	return errors.New("watch channel was closed unexpectedly")
}

func pathForKey(key []byte) string {
	return filepath.Join(*target, strings.TrimPrefix(string(key), *prefix))
}
