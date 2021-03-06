package executor

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	cp "github.com/otiai10/copy"
	"github.com/outofforest/libexec"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"

	"github.com/outofforest/isolator/client"
	"github.com/outofforest/isolator/client/wire"
	"github.com/outofforest/isolator/lib/chroot"
	"github.com/outofforest/isolator/lib/docker"
	"github.com/outofforest/isolator/lib/libhttp"
)

// Run runs isolator server
func Run(ctx context.Context) error {
	c := client.New(os.Stdin, os.Stdout)
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		spawn("watchdog", parallel.Fail, func(ctx context.Context) error {
			<-ctx.Done()
			// os.Stdin is used as input stream for client so it has to be closed to force client.Receive() to exit
			_ = os.Stdin.Close()
			return ctx.Err()
		})
		spawn("server", parallel.Exit, func(ctx context.Context) error {
			msg, err := c.Receive()
			if err != nil {
				return errors.WithStack(fmt.Errorf("fetching configuration failed: %w", err))
			}
			config, ok := msg.(wire.Config)
			if !ok {
				return errors.Errorf("expected Config message but got: %T", msg)
			}

			// creating http client before pivoting/chrooting because client reads CA certificates from system pool
			httpClient := libhttp.NewSelfClient()

			if config.Chroot {
				exitChroot, err := chroot.Enter(".")
				if err != nil {
					return errors.WithStack(err)
				}
				defer func() {
					_ = exitChroot()
				}()
			} else {
				if err := prepareNewRoot(); err != nil {
					return errors.WithStack(fmt.Errorf("preparing new root filesystem failed: %w", err))
				}
				if err := mountProc(); err != nil {
					return err
				}
				if err := mountTmp(); err != nil {
					return err
				}
				if err := populateDev(); err != nil {
					return err
				}
				if err := applyMounts(config.Mounts); err != nil {
					return errors.WithStack(fmt.Errorf("mounting host directories failed: %w", err))
				}

				if err := pivotRoot(); err != nil {
					return errors.WithStack(fmt.Errorf("pivoting root filesystem failed: %w", err))
				}

				if err := configureDNS(); err != nil {
					return err
				}
			}

			for {
				msg, err := c.Receive()
				if err != nil {
					if ctx.Err() != nil || errors.Is(err, io.EOF) {
						return ctx.Err()
					}
					return errors.WithStack(fmt.Errorf("receiving message failed: %w", err))
				}

				switch m := msg.(type) {
				case wire.Execute:
					err = execute(ctx, c, m)
				case wire.Copy:
					err = cp.Copy(m.Src, m.Dst, cp.Options{PreserveTimes: true, PreserveOwner: true})
				case wire.InitFromDocker:
					err = docker.Apply(ctx, httpClient, m.Image, m.Tag)
				default:
					return errors.Errorf("unexpected message: %T", m)
				}

				var errStr string
				if err != nil {
					errStr = err.Error()
				}
				if err := c.Send(wire.Result{
					Error: errStr,
				}); err != nil {
					return errors.WithStack(fmt.Errorf("command status reporting failed: %w", err))
				}
			}
		})
		return nil
	})
}

func execute(ctx context.Context, c *client.Client, msg wire.Execute) error {
	outTransmitter := &logTransmitter{
		stream: wire.StreamOut,
		client: c,
	}
	errTransmitter := &logTransmitter{
		stream: wire.StreamErr,
		client: c,
	}

	cmd := exec.Command("/bin/sh", "-c", msg.Command)
	cmd.Stdout = outTransmitter
	cmd.Stderr = errTransmitter

	err := libexec.Exec(ctx, cmd)

	_ = outTransmitter.Flush()
	_ = errTransmitter.Flush()

	return err
}

func prepareNewRoot() error {
	// systemd remounts everything as MS_SHARED, to rpevent mess let's remount everything back to MS_PRIVATE inside namespace
	if err := syscall.Mount("", "/", "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return errors.WithStack(fmt.Errorf("remounting / as slave failed: %w", err))
	}

	// PivotRoot can't be applied to directory where namespace was created, let's create subdirectory
	if err := os.Mkdir("root", 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}

	// PivotRoot requires new root to be on different mountpoint, so let's bind it to itself
	if err := syscall.Mount("root", "root", "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
		return errors.WithStack(fmt.Errorf("binding new root to itself failed: %w", err))
	}

	// Let's assume that new filesystem is the current working dir even before pivoting to make life easier
	return errors.WithStack(os.Chdir("root"))
}

func mountProc() error {
	if err := os.Mkdir("proc", 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", "proc", "proc", 0, ""); err != nil {
		return errors.WithStack(fmt.Errorf("mounting proc failed: %w", err))
	}
	return nil
}

func mountTmp() error {
	if err := os.Mkdir("tmp", 0o777|os.ModeSticky); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", "tmp", "tmpfs", 0, ""); err != nil {
		return errors.WithStack(fmt.Errorf("mounting tmp failed: %w", err))
	}
	return nil
}

func populateDev() error {
	devDir := "dev"
	if err := os.Mkdir(devDir, 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", devDir, "tmpfs", 0, ""); err != nil {
		return errors.WithStack(fmt.Errorf("mounting tmpfs for dev failed: %w", err))
	}
	for _, dev := range []string{"console", "null", "zero", "random", "urandom"} {
		devPath := filepath.Join(devDir, dev)
		f, err := os.OpenFile(devPath, os.O_CREATE|os.O_RDONLY, 0o644)
		if err != nil {
			return errors.WithStack(fmt.Errorf("creating dev/%s file failed: %w", dev, err))
		}
		if err := f.Close(); err != nil {
			return errors.WithStack(fmt.Errorf("closing dev/%s file failed: %w", dev, err))
		}
		if err := syscall.Mount(filepath.Join("/", devPath), devPath, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
			return errors.WithStack(fmt.Errorf("binding dev/%s device failed: %w", dev, err))
		}
	}
	links := map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "fd/0",
		"stdout": "fd/1",
		"stderr": "fd/2",
	}
	for newName, oldName := range links {
		if err := os.Symlink(oldName, devDir+"/"+newName); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func applyMounts(mounts []wire.Mount) error {
	// To mount readonly, trick is required:
	// 1. mount dir normally
	// 2. remount it using read-only option
	for _, m := range mounts {
		// force path in container should be relative to the new filesystem to prevent hacks (we haven't pivoted yet)
		m.Container = filepath.Join(".", m.Container)
		if err := os.MkdirAll(m.Container, 0o700); err != nil && !os.IsExist(err) {
			return errors.WithStack(err)
		}
		if err := syscall.Mount(m.Host, m.Container, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
			return errors.WithStack(fmt.Errorf("mounting %s to %s failed: %w", m.Host, m.Container, err))
		}
		if !m.Writable {
			if err := syscall.Mount(m.Host, m.Container, "", syscall.MS_BIND|syscall.MS_PRIVATE|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return errors.WithStack(fmt.Errorf("remounting readonly %s to %s failed: %w", m.Host, m.Container, err))
			}
		}
	}
	return nil
}

func pivotRoot() error {
	if err := os.Mkdir(".old", 0o700); err != nil {
		return errors.WithStack(err)
	}
	if err := syscall.PivotRoot(".", ".old"); err != nil {
		return errors.WithStack(fmt.Errorf("pivoting / failed: %w", err))
	}
	if err := syscall.Mount("", ".old", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return errors.WithStack(fmt.Errorf("remounting .old as private failed: %w", err))
	}
	if err := syscall.Unmount(".old", syscall.MNT_DETACH); err != nil {
		return errors.WithStack(fmt.Errorf("unmounting .old failed: %w", err))
	}
	return errors.WithStack(os.Remove(".old"))
}

func configureDNS() error {
	if err := os.Mkdir("etc", 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	return errors.WithStack(ioutil.WriteFile("etc/resolv.conf", []byte("nameserver 8.8.8.8\nnameserver 8.8.4.4\n"), 0o644))
}

type logTransmitter struct {
	client *client.Client
	stream wire.Stream

	buf []byte
}

func (lt *logTransmitter) Write(data []byte) (int, error) {
	length := len(lt.buf) + len(data)
	if length < 100 {
		lt.buf = append(lt.buf, data...)
		return len(data), nil
	}
	buf := make([]byte, length)
	copy(buf, lt.buf)
	copy(buf[len(lt.buf):], data)
	err := lt.client.Send(wire.Log{Stream: lt.stream, Text: string(buf)})
	if err != nil {
		return 0, err
	}
	lt.buf = make([]byte, 0, len(lt.buf))
	return len(data), nil
}

func (lt *logTransmitter) Flush() error {
	if len(lt.buf) == 0 {
		return nil
	}
	if err := lt.client.Send(wire.Log{Stream: lt.stream, Text: string(lt.buf)}); err != nil {
		return err
	}
	lt.buf = make([]byte, 0, len(lt.buf))
	return nil
}
