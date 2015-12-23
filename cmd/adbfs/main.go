/*
Another FUSE filesystem that can mount any device visible to your adb server.
Uses github.com/zach-klippenstein/goadb to interface with the server directly
instead of calling out to the adb client program.

See package adbfs for the filesystem implementation.
*/
package main

import (
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/hanwen/go-fuse/fuse/pathfs"
	fs "github.com/zach-klippenstein/adbfs"
	"github.com/zach-klippenstein/adbfs/internal/cli"
	"github.com/zach-klippenstein/goadb"
)

const StartTimeout = 5 * time.Second

var (
	config cli.AdbfsConfig

	server *fuse.Server

	mounted fs.AtomicBool

	// Prevents trying to unmount the server multiple times.
	unmounted fs.AtomicBool
)

func init() {
	cli.RegisterAdbfsFlags(&config)
}

func main() {
	cli.Initialize("adbfs", &config.BaseConfig)

	if config.DeviceSerial == "" {
		cli.Log.Fatalln("Device serial must be specified. Run with -h.")
	}

	if config.Mountpoint == "" {
		cli.Log.Fatalln("Mountpoint must be specified. Run with -h.")
	}
	absoluteMountpoint, err := filepath.Abs(config.Mountpoint)
	if err != nil {
		cli.Log.Fatal(err)
	}
	if err = checkValidMountpoint(absoluteMountpoint); err != nil {
		cli.Log.Fatal(err)
	}

	initializeProfiler()

	cache := initializeCache(config.CacheTtl)
	clientConfig := config.ClientConfig()

	fs := initializeFileSystem(clientConfig, absoluteMountpoint, cache, handleDeviceDisconnected)

	server, _, err = nodefs.MountRoot(absoluteMountpoint, fs.Root(), nil)
	if err != nil {
		cli.Log.Fatal(err)
	}

	serverDone, err := startServer(StartTimeout)
	if err != nil {
		cli.Log.Fatal(err)
	}
	cli.Log.Printf("mounted %s on %s", config.DeviceSerial, absoluteMountpoint)
	mounted.CompareAndSwap(false, true)
	defer unmountServer()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, os.Kill)

	for {
		select {
		case signal := <-signals:
			cli.Log.Println("got signal", signal)
			switch signal {
			case os.Kill, os.Interrupt:
				cli.Log.Println("exiting...")
				return
			}

		case <-serverDone:
			cli.Log.Debugln("server done channel closed.")
			return
		}
	}
}

func initializeProfiler() {
	if !config.ServeDebug {
		return
	}

	cli.Log.Debug("starting profiling server...")

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 0, // Bind to a random port.
	})
	if err != nil {
		cli.Log.Errorln("error starting profiling server:", err)
		return
	}

	// Publish basic table of contents.
	template, err := template.New("").Parse(`
		<html><body>
			{{range .}}
				<p><a href="{{.Path}}">{{.Text}}</a></p>
			{{end}}
		</body></html>`)
	if err != nil {
		panic(err)
	}
	toc := []struct {
		Text string
		Path string
	}{
		{"Profiling", "/debug/pprof"},
		{"Download a 30-second CPU profile", "/debug/pprof/profile"},
		{"Download a trace file (add ?seconds=x to specify sample length)", "/debug/pprof/trace"},
		{"Requests", "/debug/requests"},
		{"Event log", "/debug/events"},
	}
	http.HandleFunc("/debug", func(w http.ResponseWriter, req *http.Request) {
		template.Execute(w, toc)
	})

	go func() {
		defer listener.Close()
		if err := http.Serve(listener, nil); err != nil {
			cli.Log.Errorln("profiling server error:", err)
			return
		}
	}()

	cli.Log.Printf("profiling server listening on http://%s/debug", listener.Addr())
}

func initializeCache(ttl time.Duration) fs.DirEntryCache {
	cli.Log.Infoln("stat cache ttl:", ttl)
	return fs.NewDirEntryCache(ttl)
}

func initializeFileSystem(clientConfig goadb.ClientConfig, mountpoint string, cache fs.DirEntryCache, deviceNotFoundHandler func()) *pathfs.PathNodeFs {
	clientFactory := fs.NewCachingDeviceClientFactory(cache,
		fs.NewGoadbDeviceClientFactory(clientConfig, config.DeviceSerial))
	deviceWatcher := goadb.NewDeviceWatcher(clientConfig)

	var fsImpl pathfs.FileSystem
	fsImpl, err := fs.NewAdbFileSystem(fs.Config{
		DeviceSerial:          config.DeviceSerial,
		Mountpoint:            mountpoint,
		ClientFactory:         clientFactory,
		Log:                   cli.Log,
		ConnectionPoolSize:    config.ConnectionPoolSize,
		DeviceWatcher:         deviceWatcher,
		DeviceNotFoundHandler: deviceNotFoundHandler,
	})
	if err != nil {
		cli.Log.Fatal(err)
	}

	return pathfs.NewPathNodeFs(fsImpl, nil)
}

func startServer(startTimeout time.Duration) (<-chan struct{}, error) {
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		server.Serve()
		cli.Log.Println("server finished.")
		return
	}()

	// Wait for OS to finish initializing the mount.
	// If server.Serve() fails (e.g. mountpoint doesn't exist), WaitMount() won't
	// ever return. Running it in a separate goroutine allows us to detect that case.
	serverReady := make(chan struct{})
	go func() {
		defer close(serverReady)
		server.WaitMount()
	}()

	select {
	case <-serverReady:
		cli.Log.Println("server ready.")
		return serverDone, nil
	case <-serverDone:
		return nil, errors.New("unknown error")
	case <-time.After(startTimeout):
		return nil, errors.New(fmt.Sprint("server failed to start after", startTimeout))
	}
}

func unmountServer() {
	if server == nil {
		panic("attempted to unmount server before creating it")
	}
	if !mounted.Value() {
		panic("attempted to unmount server before mounting it")
	}

	if unmounted.CompareAndSwap(false, true) {
		cli.Log.Println("unmounting...")
		server.Unmount()
		cli.Log.Println("unmounted.")
	}
}

func handleDeviceDisconnected() {
	if !mounted.Value() || unmounted.Value() {
		// May be called before mounting if device watcher detects disconnection.
		return
	}

	cli.Log.Infoln("device disconnected, unmounting...")
	unmountServer()
}

func checkValidMountpoint(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return errors.New(fmt.Sprint("path is not a directory:", path))
	}

	return nil
}