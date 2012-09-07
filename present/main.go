// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"code.google.com/p/go.net/websocket"
)

const (
	msgLimit = 1000 // max number of messages to send per session
	basePkg  = "code.google.com/p/go.talks/present"
)

var (
	basePath   = flag.String("base", "", "base path for slide template and static resources")
	httpListen = flag.String("http", "127.0.0.1:3999", "host:port to listen on")
	uniq       = make(chan int) // a source of numbers for naming temporary files
)

func main() {
	flag.Parse()

	// source of unique numbers
	go func() {
		for i := 0; ; i++ {
			uniq <- i
		}
	}()

	if *basePath == "" {
		p, err := build.Default.Import(basePkg, "", build.FindOnly)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't find gopresent files: %v\n", err)
			fmt.Fprintf(os.Stderr, basePathMessage, basePkg)
			os.Exit(1)
		}
		*basePath = p.Dir
	}

	http.HandleFunc("/", dirHandler)
	http.Handle("/static/", http.FileServer(http.Dir(*basePath)))
	http.Handle("/socket", websocket.Handler(socketHandler))

	if !strings.HasPrefix(*httpListen, "127.0.0.1") &&
		!strings.HasPrefix(*httpListen, "localhost") {
		log.Print(localhostWarning)
	}

	log.Printf("Open your web browser and visit http://%s/", *httpListen)
	log.Fatal(http.ListenAndServe(*httpListen, nil))
}

const basePathMessage = `
By default, gopresent locates the slide template files and associated
static content by looking for a %q package
in your Go workspaces (GOPATH).

You may use the -base flag to specify an alternate location.
`

const localhostWarning = `
WARNING!  WARNING!  WARNING!

The present server appears to be listening on an address that is not localhost.
Anyone with access to this address and port will have access to this machine as
the user running present.

If you don't understand this message, hit Control-C to terminate this process.

WARNING!  WARNING!  WARNING!
`

// dirHandler serves a directory listing for the requested path, rooted at basePath.
func dirHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/favicon.ico" {
		http.Error(w, "not found", 404)
		return
	}
	const base = "."
	name := filepath.Join(base, r.URL.Path)
	if filepath.Ext(name) == ".slide" {
		err := renderSlides(w, *basePath, name)
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), 500)
		}
		return
	}
	if isDir, err := dirList(w, name); err != nil {
		log.Println(err)
		http.Error(w, err.Error(), 500)
		return
	} else if isDir {
		return
	}
	http.FileServer(http.Dir(base)).ServeHTTP(w, r)
}

// Message is the wire format for the websocket connection to the browser.
// It is used for both sending output messages and receiving commands, as
// distinguished by the Kind field.
type Message struct {
	Id   string // client-provided unique id for the process
	Kind string // in: "run", "kill" out: "stdout", "stderr", "end"
	Body string
}

// socketHandler handles the websocket connection for given present session.
// It constructs a new Client and handles transcoding Messages to and from JSON
// format, sending and receiving those messages on the Client's in and out
// channels.
func socketHandler(conn *websocket.Conn) {
	in, out := make(chan *Message), make(chan *Message)
	c := &Client{
		proc: make(map[string]*Process),
		in:   in,
		out:  out,
	}
	go c.loop()

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	errc := make(chan error, 1)
	go func() {
		for {
			m := new(Message)
			if err := dec.Decode(m); err != nil {
				errc <- err
				close(in)
				return
			}
			in <- m
		}
	}()
	go func() {
		counts := make(map[string]int)
		for {
			m := <-out
			switch c := counts[m.Id]; {
			case m.Kind == "end" || c < msgLimit:
				if err := enc.Encode(m); err != nil {
					errc <- err
					return
				}
			case c == msgLimit:
				// Kill it.
				in <- &Message{Id: m.Id, Kind: "end"}
			}
			counts[m.Id]++
		}
	}()
	err := <-errc
	if err != nil && err != io.EOF {
		log.Println(err)
	}
}

// Client represents a connected present client.
// It manages any processes being compiled and run for the client.
type Client struct {
	sync.Mutex
	proc map[string]*Process
	in   <-chan *Message
	out  chan<- *Message
}

// loop handles incoming messages from the client.
func (c *Client) loop() {
	for m := range c.in {
		switch m.Kind {
		case "run":
			c.kill(m.Id)
			c.run(m.Id, m.Body)
		case "kill":
			c.kill(m.Id)
		}
	}
}

// kill shuts down a running process.
func (c *Client) kill(id string) {
	c.Lock()
	if p := c.proc[id]; p != nil {
		p.kill()
		delete(c.proc, id)
	}
	c.Unlock()
}

// run compiles and runs the given program, associating it with the given id.
func (c *Client) run(id, body string) {
	go func() {
		p := &Process{
			id:     id,
			stdout: newPiper(id, "stdout", c.out),
			stderr: newPiper(id, "stderr", c.out),
		}
		c.Lock()
		c.proc[id] = p
		c.Unlock()
		err := p.run(body)
		m := &Message{Id: id, Kind: "end"}
		if err != nil {
			m.Body = err.Error()
		}
		c.Lock()
		delete(c.proc, id)
		c.Unlock()
		c.out <- m
	}()
}

// Process represents a running process.
type Process struct {
	sync.Mutex     // guards cmd
	id             string
	stdout, stderr io.Writer
	cmd            *exec.Cmd
}

var tmpdir string

func init() {
	// find real temporary directory (for rewriting filename in output)
	var err error
	tmpdir, err = filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		log.Fatal(err)
	}
}

// run compiles and runs the given go program.
func (p *Process) run(body string) error {
	// x is the base name for .go and executable files
	x := filepath.Join(tmpdir, "compile"+strconv.Itoa(<-uniq))
	src := x + ".go"
	bin := x
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	// write body to x.go
	defer os.Remove(src)
	if err := ioutil.WriteFile(src, []byte(body), 0666); err != nil {
		return err
	}

	// build x.go, creating x
	dir, file := filepath.Split(src)
	err := p.exec(dir, "go", "build", "-o", bin, file)
	defer os.Remove(bin)
	if err != nil {
		return err
	}

	// run x
	return p.exec("", bin)
}

// exec runs the specified command in the given directory, writing all standard
// output and standard error to the Process' stdout and stderr fields. It
// stores the running command in the cmd field, and returns when the command
// exits.
func (p *Process) exec(dir string, args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Stdout = p.stdout
	cmd.Stderr = p.stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	p.Lock()
	p.cmd = cmd
	p.Unlock()

	err := cmd.Wait()

	p.Lock()
	p.cmd = nil
	p.Unlock()

	return err
}

// kill stops the process if it is running.
func (p *Process) kill() {
	p.Lock()
	if p.cmd != nil {
		p.cmd.Process.Kill()
		p.cmd = nil
	}
	p.Unlock()
}

// newPiper returns a writer which converts all writes to Message sends on the
// given channel with the specified id and kind. It splits on newlines.
func newPiper(id, kind string, out chan<- *Message) io.Writer {
	r, w := io.Pipe()
	p := &piper{id, kind, bufio.NewReaderSize(r, 80), out}
	go p.read()
	return w
}

type piper struct {
	id, kind string
	r        *bufio.Reader
	out      chan<- *Message
}

func (p *piper) read() {
	for {
		m := &Message{Id: p.id, Kind: p.kind}
		l, prefix, err := p.r.ReadLine()
		if err != nil {
			return
		}
		m.Body = string(l)
		if !prefix {
			m.Body += "\n"
		}
		p.out <- m
	}
}