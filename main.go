package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jille/dfr"
	clientconfig "github.com/Jille/etcd-client-from-env"
	"github.com/spf13/pflag"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

var (
	prefix       = pflag.StringP("prefix", "p", "", "Prefix of etcd files to fetch")
	target       = pflag.StringP("target", "t", "", "Target directory to install etcd content to")
	reloadSignal = pflag.StringP("signal", "s", "", "Signal to send when config files are changed")
	mode         = pflag.StringP("mode", "m", "600", "Mode (as in chmod) of the created files")
	owner        = pflag.StringP("owner", "o", "", "owner of the created files")
	group        = pflag.StringP("group", "g", "", "group of the created files")
)

var (
	c         *clientv3.Client
	sigCh     = make(chan os.Signal, 1000)
	reloadSig os.Signal
	perms     permissions
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	ctx := context.Background()
	pflag.Parse()
	*prefix = strings.TrimSuffix(*prefix, "/") + "/"
	var err error
	perms, err = parsePermissions()
	if err != nil {
		log.Fatal(err)
	}
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
	cc.DialOptions = append(cc.DialOptions, grpc.WithBlock())
	c, err = clientv3.New(cc)
	if err != nil {
		log.Fatalf("Failed to connect to etcd: %v", err)
	}
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
		if err := perms.WriteFile(pathForKey(kv.Key), kv.Value); err != nil {
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
				if err := perms.WriteFile(pathForKey(e.Kv.Key), e.Kv.Value); err != nil {
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

type permissions struct {
	mode  os.FileMode
	owner int
	group int
}

func parsePermissions() (permissions, error) {
	ret := permissions{
		mode:  0600,
		owner: -1,
		group: -1,
	}
	if *mode != "" {
		m, err := strconv.ParseUint(*mode, 8, 8)
		if err != nil {
			return ret, fmt.Errorf("given --mode (%q) is not valid", *mode)
		}
		ret.mode = os.FileMode(m)
	}
	if *owner != "" {
		u, err := user.Lookup(*owner)
		if err != nil {
			return ret, fmt.Errorf("failed to look up user %q: %v", *owner, err)
		}
		uid, err := strconv.ParseInt(u.Uid, 10, 64)
		if err != nil {
			return ret, errors.New("uid for owner is not numeric")
		}
		ret.owner = int(uid)
	}
	if *group != "" {
		g, err := user.Lookup(*group)
		if err != nil {
			return ret, fmt.Errorf("failed to look up group %q: %v", *group, err)
		}
		gid, err := strconv.ParseInt(g.Gid, 10, 64)
		if err != nil {
			return ret, errors.New("gid for group is not numeric")
		}
		ret.group = int(gid)
	}
	return ret, nil
}

func (p permissions) WriteFile(fn string, data []byte) (retErr error) {
	var d dfr.D
	defer d.Run(&retErr)
	fh, err := os.CreateTemp(filepath.Dir(fn), "")
	if err != nil {
		return err
	}
	unlinker := d.AddErr(func() error {
		return os.Remove(fh.Name())
	})
	closer := d.AddErr(fh.Close)
	if p.owner != -1 || p.group != -1 {
		if err := os.Chown(fh.Name(), p.owner, p.group); err != nil {
			return err
		}
	}
	if err := os.Chmod(fh.Name(), p.mode); err != nil {
		return err
	}
	if _, err := fh.Write(data); err != nil {
		return err
	}
	if err := closer(true); err != nil {
		return err
	}
	if err := os.Rename(fh.Name(), fn); err != nil {
		return err
	}
	unlinker(false)
	return nil
}
