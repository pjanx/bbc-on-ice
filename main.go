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

const (
	targetURI = "http://as-hls-%s-live.akamaized.net/pool_904/live/%s/" +
		"%s/%s.isml/%s-audio%%3d%s.norewind.m3u8"
	metaURI = "https://rms.api.bbc.co.uk/v2/services/%s/segments/latest"
)

type meta struct {
	title   string // what's playing right now
	timeout uint   // timeout for the next poll in ms
}

// getMeta retrieves and decodes metadata info from an independent webservice.
func getMeta(name string) (*meta, error) {
	resp, err := http.Get(fmt.Sprintf(metaURI, name))
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(resp.Body)

	// TODO: update more completely for the new OpenAPI
	//  - `broadcasts/poll/bbc_radio_one` looks almost useful
	//  - https://rms.api.bbc.co.uk/v2/experience/inline/play/${name}
	//    seems to be what we want, even provides timer/polling values
	var v struct {
		Data []struct {
			Titles struct {
				Primary   string  `json:"primary"`
				Secondary *string `json:"secondary"`
				Tertiary  *string `json:"tertiary"`
			} `json:"titles"`
			Offset struct {
				NowPlaying bool `json:"now_playing"`
			} `json:"offset"`
		} `json:"data"`
	}
	err = json.Unmarshal(b, &v)
	if err != nil {
		return nil, errors.New("invalid metadata response")
	}
	if len(v.Data) == 0 || !v.Data[0].Offset.NowPlaying {
		return nil, errors.New("no song is playing")
	}

	titles := v.Data[0].Titles
	parts := []string{}
	if titles.Tertiary != nil {
		parts = append(parts, *titles.Tertiary)
	}
	if titles.Secondary != nil {
		parts = append(parts, *titles.Secondary)
	}
	parts = append(parts, titles.Primary)
	return &meta{timeout: 5000, title: strings.Join(parts, " - ")}, nil
}

// resolveM3U8 resolves an M3U8 playlist to the first link that seems to
// be playable, possibly recursing.
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
			// Seems to be a relative link, let's make it absolute.
			dir, _ := path.Split(target)
			line = dir + line
		}
		if strings.HasSuffix(line, "m3u8") {
			// The playlist seems to recurse, and so will we.
			// XXX: This should be bounded, not just by the stack.
			return resolveM3U8(line)
		}
		out = append(out, line)
	}
	return out, nil
}

// metaProc periodically polls the sub-URL given by name for titles and sends
// them out the given channel. Never returns prematurely.
func metaProc(ctx context.Context, name string, out chan<- string) {
	defer close(out)

	// "polling_timeout" seems to normally be 25 seconds, which is a lot,
	// especially considering all the possible additional buffering.
	const maxInterval = 5 * time.Second

	var current, last string
	var interval time.Duration
	for {
		meta, err := getMeta(name)
		if err != nil {
			current = name + " - " + err.Error()
			interval = maxInterval
		} else {
			current = meta.title
			interval = time.Duration(meta.timeout) * time.Millisecond
			if interval > maxInterval {
				interval = maxInterval
			}
		}
		if current != last {
			select {
			case out <- current:
			case <-ctx.Done():
				return
			}
			last = current
		}

		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return
		}
	}
}

// urlProc periodically checks the playlist for yet unseen URLs and sends them
// over the channel. Assumes that URLs are incremental for simplicity, although
// there doesn't seem to be any such gaurantee by the HLS protocol.
func urlProc(ctx context.Context, playlistURL string, out chan<- string) {
	defer close(out)

	highest := ""
	for {
		target, err := resolveM3U8(playlistURL)
		if err != nil {
			return
		}
		for _, url := range target {
			if url <= highest {
				continue
			}
			select {
			case out <- url:
				highest = url
			case <-ctx.Done():
				return
			}
		}
		// Media players will happily buffer the whole playlist at once,
		// a small (less than target duration) additional pause is appropriate.
		time.Sleep(3 * time.Second)
	}
}

// https://tools.ietf.org/html/rfc8216
// http://www.gpac-licensing.com/2014/12/08/apple-hls-technical-depth/
func dataProc(ctx context.Context, playlistURL string, maxChunk int,
	out chan<- []byte) {
	defer close(out)

	// The channel is buffered so that the urlProc can fetch in advance.
	urls := make(chan string, 3)
	go urlProc(ctx, playlistURL, urls)

	for url := range urls {
		resp, err := http.Get(url)
		if resp != nil {
			defer resp.Body.Close()
		}
		if err != nil {
			return
		}

		for {
			chunk := make([]byte, maxChunk)
			n, err := resp.Body.Read(chunk)

			select {
			case out <- chunk[:n]:
			case <-ctx.Done():
				return
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return
			}
		}
	}
}

var pathRE = regexp.MustCompile(`^/(.*?)/(.*?)/(.*?)$`)

func proxy(w http.ResponseWriter, req *http.Request) {
	const metaint = 1 << 15
	m := pathRE.FindStringSubmatch(req.URL.Path)
	if m == nil {
		http.NotFound(w, req)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		// We're not using TLS where HTTP/2 could have caused this.
		panic("cannot hijack connection")
	}

	// [ww]/[uk], [48000/96000]/[128000/320000], bbc_radio_one/bbc_1xtra/...
	region, quality, name := m[1], m[2], m[3]

	mediaPlaylistURL :=
		fmt.Sprintf(targetURI, region, region, name, name, name, quality)

	// This validates the parameters as a side-effect.
	media, err := resolveM3U8(mediaPlaylistURL)
	if err == nil && len(media) == 0 {
		err = errors.New("cannot resolve playlist")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	wantMeta := req.Header.Get("Icy-MetaData") == "1"
	resp, err := http.Head(media[0])
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

	// TODO: Retrieve some general information from somewhere?
	// There's nothing interesting in the playlist files.

	fmt.Fprintf(bufrw, "ICY 200 OK\r\n")
	fmt.Fprintf(bufrw, "icy-name:%s\r\n", name)
	// BBC marks this as a video type, maybe just force audio/mpeg.
	fmt.Fprintf(bufrw, "content-type:%s\r\n", resp.Header["Content-Type"][0])
	fmt.Fprintf(bufrw, "icy-pub:%d\r\n", 0)
	if wantMeta {
		fmt.Fprintf(bufrw, "icy-metaint: %d\r\n", metaint)
	}
	fmt.Fprintf(bufrw, "\r\n")

	metaChan := make(chan string)
	go metaProc(req.Context(), name, metaChan)

	chunkChan := make(chan []byte)
	go dataProc(req.Context(), mediaPlaylistURL, metaint, chunkChan)

	// dataProc may return less data near the end of a subfile, so we give it
	// a maximum count of bytes to return at once and do our own buffering.
	var queuedMetaUpdate, queuedData []byte
	writeMeta := func() error {
		if !wantMeta {
			return nil
		}

		var meta [1 + 16*255]byte
		meta[0] = byte((copy(meta[1:], queuedMetaUpdate) + 15) / 16)
		queuedMetaUpdate = nil

		_, err := bufrw.Write(meta[:1+int(meta[0])*16])
		return err
	}
	for {
		select {
		case title := <-metaChan:
			queuedMetaUpdate = []byte(fmt.Sprintf("StreamTitle='%s'",
				strings.Replace(title, "'", "’", -1)))
		case chunk, ok := <-chunkChan:
			if !ok {
				return
			}
			missing := metaint - len(queuedData)
			if len(chunk) < missing {
				queuedData = append(queuedData, chunk...)
				continue
			}
			queuedData = append(queuedData, chunk[:missing]...)
			if _, err := bufrw.Write(queuedData); err != nil {
				return
			}
			queuedData = chunk[missing:]
			if writeMeta() != nil || bufrw.Flush() != nil {
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
		log.Println("LISTEN_FDS unworkable")
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

// Had to copy this from Server.ListenAndServe.
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
	var listener net.Listener
	if ln := socketActivationListener(); ln != nil {
		// Keepalives can be set in the systemd unit, see systemd.socket(5).
		listener = ln
	} else {
		if len(os.Args) < 2 {
			log.Fatalf("usage: %s LISTEN-ADDR\n", os.Args[0])
		}
		if ln, err := net.Listen("tcp", os.Args[1]); err != nil {
			log.Fatalln(err)
		} else {
			listener = tcpKeepAliveListener{ln.(*net.TCPListener)}
		}
	}

	http.HandleFunc("/", proxy)
	// We don't need to clean up properly since we store no data.
	log.Fatalln(http.Serve(listener, nil))
}
