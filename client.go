package sftp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/nethack42/go-sftp/sshfxp"
)

type Client struct {
	reader io.ReadCloser
	writer io.WriteCloser

	incoming chan sshfxp.Packet
	outgoing chan sshfxp.Packet
	errch    chan error
	ioErr    error

	router *Router

	version uint32

	wg sync.WaitGroup
}

func NewClient(r io.ReadCloser, w io.WriteCloser) *Client {
	cli := &Client{
		reader:   r,
		writer:   w,
		incoming: make(chan sshfxp.Packet),
		outgoing: make(chan sshfxp.Packet),
		router:   NewRouter(),
		errch:    make(chan error, 2), // one error per goroutine
	}

	cli.wg.Add(2)
	go func(cli *Client) {
		defer cli.wg.Done()
		defer logrus.Infof("SFTP client writer exited")

		cli.errch <- writeConn(cli.writer, cli.outgoing)
	}(cli)

	go func(cli *Client) {
		defer cli.wg.Done()
		defer logrus.Infof("SFTP client reader exited")

		cli.errch <- readConn(cli.reader, cli.incoming)
	}(cli)

	if err := cli.DoHandshake(); err != nil {
		logrus.Errorf("SFTP handshake failed: %s", err)

		// Close outgoing
		close(cli.outgoing)

		cli.reader.Close()
		cli.writer.Close()

		cli.wg.Wait()

		cli.ioErr = err

		return nil
	}

	logrus.Infof("SFTP-handeshake complete. Using SFTP version %d", cli.version)

	cli.wg.Add(1)

	go func(cli *Client) {
		defer cli.wg.Done()

	L:
		for {
			select {
			case msg := <-cli.incoming:
				// TODO we currently ignore any error from message handling
				if err := cli.handleMessage(msg); err != nil {
					logrus.Errorf("failed to handle message: %s", err)
				}

			case err := <-cli.errch:
				if err != nil {
					cli.ioErr = err
					logrus.Errorf("received error: %s", err)
				}
				logrus.Infof("received nil on errch")
				break L
			}
		}

		close(cli.outgoing) // will cause writer to stop if it hasn't already

	}(cli)

	return cli
}

// Wait waits for hte cient goroutines to finish
func (cli *Client) Wait() {
	cli.wg.Wait()
}

func (cli *Client) send(x sshfxp.Message) (<-chan sshfxp.Message, error) {
	var pkt sshfxp.Packet
	var res <-chan sshfxp.Message

	if header, ok := (interface{}(x)).(sshfxp.Header); ok {
		id, ch := cli.router.Get()

		header.SetID(id)

		res = ch
	}

	if err := pkt.Encode(x); err != nil {
		return nil, err
	}

	cli.outgoing <- pkt

	return res, nil
}

func (cli *Client) handleMessage(msg sshfxp.Packet) error {
	logrus.Infof("Got message: Len=%d Type=%d: %#v", msg.Length, msg.Type, msg)
	payload, err := msg.Decode()
	if err != nil {
		return fmt.Errorf("failed to decode message: %s", err)
	}

	if err := cli.router.Resolve(payload); err != nil {
		return err
	}

	return nil
}

func (cli *Client) DoHandshake() error {
	init := &sshfxp.Init{
		Version: 3,
	}

	if _, err := cli.send(init); err != nil {
		return err
	}

	pkt := <-cli.incoming

	msg, err := pkt.Decode()
	if err != nil {
		return err
	}

	if version, ok := msg.(*sshfxp.Version); !ok {
		return errors.New("unexpected message received")
	} else {
		if version.Version != init.Version {
			return errors.New("unsupported version")
		}

		cli.version = version.Version
	}

	return nil
}

func (cli *Client) Version() uint32 {
	return cli.version
}

func (cli *Client) OpenDir(path string) (string, error) {
	open := &sshfxp.OpenDir{
		Path: path,
	}

	var err error
	var res_chan <-chan sshfxp.Message

	if res_chan, err = cli.send(open); err != nil {
		return "", err
	}

	// wait for result
	var res interface{} = <-res_chan

	switch msg := res.(type) {
	case *sshfxp.Handle:
		return msg.Handle, nil
	case *sshfxp.Status:
		return "", fmt.Errorf("%d - %s", msg.Error, msg.Message)
	}

	return "", fmt.Errorf("unexpected response: %#v", res)
}

func (cli *Client) ReadDir(handle string) ([]*os.FileInfo, error) {
	read := &sshfxp.ReadDir{
		Handle: handle,
	}

	resCh, err := cli.send(read)
	if err != nil {
		return nil, err
	}

	res := <-resCh

	logrus.Infof("Got: %#v", res)
	return nil, nil
}
