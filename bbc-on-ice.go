package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
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
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if len(b) < 2 {
		// There needs to be an enclosing () pair
		return nil, errors.New("invalid metadata response")
	}

	// TODO: also retrieve richtracks/is_now_playing, see example file
	type broadcast struct {
		Title      string `json:"title"`      // Title of the broadcast
		Percentage int    `json:"percentage"` // How far we're in
	}
	var v struct {
		Packages struct {
			OnAir struct {
				Broadcasts        []broadcast `json:"broadcasts"`
				BroadcastNowIndex uint        `json:"broadcastNowIndex"`
			} `json:"on-air"`
		} `json:"packages"`
		Timeouts struct {
			PollingTimeout uint `json:"polling_timeout"`
		} `json:"timeouts"`
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
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
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

var pathRE = regexp.MustCompile("^/(.*?)/(.*?)/(.*?)$")

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

	// TODO: move to a normal function
	metaChan := make(chan string)
	go func() {
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
				metaChan <- current
				last = current
			}

			select {
			case <-time.After(time.Duration(interval) * time.Millisecond):
			case <-req.Context().Done():
				return
			}
		}
	}()

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
	makeMetaChunk := func() []byte {
		meta := queuedMeta
		queuedMeta = nil
		for len(meta)%16 != 0 {
			meta = append(meta, 0)
		}
		if len(meta) > 16*255 {
			meta = meta[:16*255]
		}
		chunk := []byte{byte(len(meta) / 16)}
		return append(chunk, meta...)
	}

	for {
		select {
		case title := <-metaChan:
			queuedMeta = []byte(fmt.Sprintf("StreamTitle='%s'", title))
		case chunk, connected := <-chunkChan:
			if !connected {
				return
			}
			if wantMeta {
				chunk = append(chunk, makeMetaChunk()...)
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

func main() {
	// TODO: also try to support systemd socket activation
	address := ":8000"
	if len(os.Args) == 2 {
		address = os.Args[1]
	}

	http.HandleFunc("/", proxy)
	log.Fatal(http.ListenAndServe(address, nil))
}
