package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/internal/cli/server/proto"
	"github.com/golang/protobuf/jsonpb"
	gproto "github.com/golang/protobuf/proto"
	"github.com/mitchellh/cli"
	grpc_net_conn "github.com/mitchellh/go-grpc-net-conn"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/runtime/protoiface"
)

// DebugCommand is the command to group the peers commands
type DebugCommand struct {
	UI cli.Ui
}

// Help implements the cli.Command interface
func (c *DebugCommand) Help() string {
	return `Usage: bor peers <subcommand>

  This command groups actions to interact with peers.
	
  List the connected peers:
  
    $ bor peers list
	
  Add a new peer by enode:
  
    $ bor peers add <enode>

  Remove a connected peer by enode:

    $ bor peers remove <enode>

  Display information about a peer:

    $ bor peers status <peer id>`
}

// Synopsis implements the cli.Command interface
func (c *DebugCommand) Synopsis() string {
	return "Interact with peers"
}

// Run implements the cli.Command interface
func (c *DebugCommand) Run(args []string) int {
	return cli.RunResultHelp
}

type debugEnv struct {
	output string
	prefix string

	name string
	dst  string
}

func (d *debugEnv) init() error {
	d.name = d.prefix + time.Now().UTC().Format("2006-01-02-150405Z")

	var err error

	// Create the output directory
	var tmp string
	if d.output != "" {
		// User specified output directory
		tmp = filepath.Join(d.output, d.name)
		_, err := os.Stat(tmp)
		if !os.IsNotExist(err) {
			return fmt.Errorf("output directory already exists")
		}
	} else {
		// Generate temp directory
		tmp, err = ioutil.TempDir(os.TempDir(), d.name)
		if err != nil {
			return fmt.Errorf("error creating tmp directory: %s", err.Error())
		}
	}

	// ensure destine folder exists
	if err := os.MkdirAll(tmp, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create parent directory: %v", err)
	}

	d.dst = tmp
	return nil
}

func (d *debugEnv) tarName() string {
	return d.name + ".tar.gz"
}

func (d *debugEnv) finish() error {
	// Exit before archive if output directory was specified
	if d.output != "" {
		return nil
	}

	// Create archive tarball
	archiveFile := d.name + ".tar.gz"
	if err := tarCZF(archiveFile, d.dst, d.name); err != nil {
		return fmt.Errorf("error creating archive: %s", err.Error())
	}
	return nil
}

type debugStream interface {
	Recv() (*proto.DebugFileResponse, error)
	grpc.ClientStream
}

func (d *debugEnv) writeFromStream(name string, stream debugStream) error {
	// wait for open request
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	if _, ok := msg.Event.(*proto.DebugFileResponse_Open_); !ok {
		return fmt.Errorf("expected open message")
	}

	// create the stream
	conn := &grpc_net_conn.Conn{
		Stream:   stream,
		Response: &proto.DebugFileResponse_Input{},
		Decode: grpc_net_conn.SimpleDecoder(func(msg gproto.Message) *[]byte {
			return &msg.(*proto.DebugFileResponse_Input).Data
		}),
	}

	file, err := os.OpenFile(filepath.Join(d.dst, name), os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := io.Copy(file, conn); err != nil {
		return err
	}
	return nil
}

func (d *debugEnv) writeJSON(name string, msg protoiface.MessageV1) error {
	m := jsonpb.Marshaler{}
	data, err := m.MarshalToString(msg)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(d.dst, name), []byte(data), 0644); err != nil {
		return fmt.Errorf("failed to write status: %v", err)
	}
	return nil
}

func trapSignal(cancel func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	go func() {
		<-sigCh
		cancel()
	}()
}

func tarCZF(archive string, src, target string) error {
	// ensure the src actually exists before trying to tar it
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("unable to tar files - %v", err.Error())
	}

	// create the archive
	fh, err := os.Create(archive)
	if err != nil {
		return err
	}
	defer fh.Close()

	zz := gzip.NewWriter(fh)
	defer zz.Close()

	tw := tar.NewWriter(zz)
	defer tw.Close()

	// tar
	return filepath.Walk(src, func(file string, fi os.FileInfo, err error) error {
		// return on any error
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}

		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		// remove leading path to the src, so files are relative to the archive
		path := strings.ReplaceAll(file, src, "")
		if target != "" {
			path = filepath.Join([]string{target, path}...)
		}
		path = strings.TrimPrefix(path, string(filepath.Separator))

		header.Name = path

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// copy the file contents
		f, err := os.Open(file)
		if err != nil {
			return err
		}

		if _, err := io.Copy(tw, f); err != nil {
			return err
		}

		f.Close()
		return nil
	})
}
