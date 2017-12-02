package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type meta struct {
	title   string // What's playing right now
	timeout uint   // Timeout for the next poll in ms
}

// Retrieve and decode metadata information from an independent webservice
func getMeta(name string) (*meta, error) {
	const metaBaseURI = "http://polling.bbc.co.uk/radio/nhppolling/"
	resp, err := http.Get(metaBaseURI + name)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(resp.Body)
	if len(b) < 2 {
		// There needs to be an enclosing () pair
		return nil, errors.New("invalid metadata response")
	}

	// TODO: also retrieve richtracks/is_now_playing, see example file
	type broadcast struct {
		Title      string // Title of the broadcast
		Percentage int    // How far we're in
	}
	var v struct {
		Packages struct {
			OnAir struct {
				Broadcasts        []broadcast
				BroadcastNowIndex uint
			} `json:"on-air"`
		}
		Timeouts struct {
			PollingTimeout uint `json:"polling_timeout"`
		}
	}
	err = json.Unmarshal(b[1:len(b)-1], &v)
	if err != nil {
		return nil, errors.New("invalid metadata response")
	}
	onAir := v.Packages.OnAir
	if onAir.BroadcastNowIndex >= uint(len(onAir.Broadcasts)) {
		return nil, errors.New("no active broadcast")
	}
	return &meta{
		timeout: v.Timeouts.PollingTimeout,
		title:   onAir.Broadcasts[onAir.BroadcastNowIndex].Title,
	}, nil
}

// Resolve an M3U8 playlist to the first link that seems to be playable
func resolveM3U8(target string) (out []string, err error) {
	resp, err := http.Get(target)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(resp.Body)
	if !utf8.Valid(b) {
		return nil, errors.New("invalid UTF-8")
	}
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, "/") {
			// Seems to be a relative link, let's make it absolute
			dir, _ := path.Split(target)
			line = dir + line
		}
		if strings.HasSuffix(line, "m3u8") {
			// The playlist seems to recurse, and so do we
			return resolveM3U8(line)
		}
		out = append(out, line)
	}
	return out, nil
}

func metaProc(ctx context.Context, name string, out chan<- string) {
	defer close(out)

	var current, last string
	var interval time.Duration
	for {
		meta, err := getMeta(name)
		if err != nil {
			current = "Error: " + err.Error()
			interval = 30 * time.Second
		} else {
			current = meta.title
			interval = time.Duration(meta.timeout)
		}
		if current != last {
			// TODO: see https://blog.golang.org/pipelines
			//   find out if we can do this better
			select {
			case out <- current:
			case <-ctx.Done():
				return
			}
			last = current
		}

		select {
		case <-time.After(time.Duration(interval) * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

var pathRE = regexp.MustCompile(`^/(.*?)/(.*?)/(.*?)$`)

func proxy(w http.ResponseWriter, req *http.Request) {
	const targetURI = "http://a.files.bbci.co.uk/media/live/manifesto/" +
		"audio/simulcast/hls/%s/%s/ak/%s.m3u8"
	const metaint = 1 << 16
	m := pathRE.FindStringSubmatch(req.URL.Path)
	if m == nil {
		http.NotFound(w, req)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		// We're not using TLS where HTTP/2 could have caused this
		panic("cannot hijack connection")
	}

	// E.g. `nonuk`, `sbr_low` `bbc_radio_one`, or `uk`, `sbr_high`, `bbc_1xtra`
	region, quality, name := m[1], m[2], m[3]
	// This validates the params as a side-effect
	target, err := resolveM3U8(fmt.Sprintf(targetURI, region, quality, name))
	if err == nil && len(target) == 0 {
		err = errors.New("cannot resolve playlist")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	wantMeta := false
	if icyMeta, ok := req.Header["Icy-MetaData"]; ok {
		wantMeta = len(icyMeta) == 1 && icyMeta[0] == "1"
	}
	resp, err := http.Get(target[0])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// TODO: retrieve some general information from somewhere?
	//   There's nothing interesting in the playlist files.
	fmt.Fprintf(bufrw, "ICY 200 OK\r\n")
	fmt.Fprintf(bufrw, "icy-name:%s\r\n", name)
	// BBC marks this as a video type, maybe just force audio/mpeg
	fmt.Fprintf(bufrw, "content-type:%s\r\n", resp.Header["Content-Type"][0])
	fmt.Fprintf(bufrw, "icy-pub:%d\r\n", 0)
	if wantMeta {
		fmt.Fprintf(bufrw, "icy-metaint: %d\r\n", metaint)
	}
	fmt.Fprintf(bufrw, "\r\n")

	metaChan := make(chan string)
	go metaProc(req.Context(), name, metaChan)

	// TODO: move to a normal function
	// FIXME: this will load a few seconds (one URL) and die
	//   - we can either try to implement this and hope for the best
	//     https://tools.ietf.org/html/draft-pantos-http-live-streaming-20
	//     then like https://github.com/kz26/gohls/blob/master/main.go
	//   - or we can become more of a proxy, which complicates ICY
	chunkChan := make(chan []byte)
	go func() {
		defer resp.Body.Close()
		defer close(chunkChan)
		for {
			chunk := make([]byte, metaint)
			n, err := io.ReadFull(resp.Body, chunk)
			chunkChan <- chunk[:n]
			if err != nil {
				return
			}

			select {
			default:
			case <-req.Context().Done():
				return
			}
		}
	}()

	var queuedMeta []byte
	for {
		select {
		case title := <-metaChan:
			queuedMeta = []byte(fmt.Sprintf("StreamTitle='%s'", title))
		case chunk, ok := <-chunkChan:
			if !ok {
				return
			}
			if wantMeta {
				var meta [1 + 16*255]byte
				meta[0] = byte((copy(meta[1:], queuedMeta) + 15) / 16)
				chunk = append(chunk, meta[:1+int(meta[0])*16]...)
				queuedMeta = nil
			}
			if _, err := bufrw.Write(chunk); err != nil {
				return
			}
			if err := bufrw.Flush(); err != nil {
				return
			}
		}
	}
}

// https://www.freedesktop.org/software/systemd/man/sd_listen_fds.html
func socketActivationListener() net.Listener {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil
	}

	nfds, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || nfds == 0 {
		return nil
	} else if nfds > 1 {
		log.Fatalln("not supporting more than one listening socket")
	}

	const firstListenFd = 3
	syscall.CloseOnExec(firstListenFd)
	ln, err := net.FileListener(os.NewFile(firstListenFd, "socket activation"))
	if err != nil {
		log.Fatalln(err)
	}
	return ln
}

// Had to copy this from Server.ListenAndServe()
type tcpKeepAliveListener struct{ *net.TCPListener }

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}

func main() {
	listenAddr := ":8000"
	if len(os.Args) == 2 {
		listenAddr = os.Args[1]
	}

	var listener net.Listener
	if ln := socketActivationListener(); listener != nil {
		// Keepalives can be set in the systemd unit, see systemd.socket(5)
		listener = ln
	} else if ln, err := net.Listen("tcp", listenAddr); err != nil {
		log.Fatalln(err)
	} else {
		listener = tcpKeepAliveListener{ln.(*net.TCPListener)}
	}

	http.HandleFunc("/", proxy)
	// We don't need to clean up properly since we store no data
	log.Fatalln(http.Serve(listener, nil))
}
