package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/urfave/cli/v2"
	"go.uber.org/zap"
	"golang.org/x/crypto/acme/autocert"
)

var seq uint64 // guarded by atomic

var serverFlags struct {
	secure  bool
	host    string
	maxSize int64
	dir     string
}

var serverCmd = &cli.Command{
	Name:        "server",
	Description: "starts a reports upload server",
	Action:      runReportsServer,
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:        "secure",
			Usage:       "whether to start with TLS and an autocert-managed certificate",
			Value:       true,
			Destination: &serverFlags.secure,
		},
		&cli.StringFlag{
			Name:        "host",
			Usage:       "hostname to request certificate for in secure mode",
			Destination: &serverFlags.host,
		},
		&cli.Int64Flag{
			Name:        "max-size",
			Usage:       "max size of upload in bytes",
			Value:       32 << 20, // 32MiB
			Destination: &serverFlags.maxSize,
		},
		&cli.StringFlag{
			Name:        "dir",
			Usage:       "root of working directory, where the reports will be deposited and the TLS certificates will be kept, in different subdirectories",
			Destination: &serverFlags.dir,
			Required:    true,
		},
	},
}

func runReportsServer(_ *cli.Context) error {
	if serverFlags.secure && serverFlags.host == "" {
		return fmt.Errorf("host required when running in secure mode")
	}

	log.Infof("starting server")

	keysdir := filepath.Join(serverFlags.dir, "keys")
	if err := os.MkdirAll(keysdir, 0755); err != nil {
		log.Fatalf("failed to create directory", "dir", keysdir, "error", err)
	}
	datadir := filepath.Join(serverFlags.dir, "data")
	if err := os.MkdirAll(datadir, 0755); err != nil {
		log.Fatalf("failed to create directory", "dir", datadir, "error", err)
	}

	var m *autocert.Manager
	mux := new(http.ServeMux)
	mux.HandleFunc("/upload", handleUpload(datadir))

	if serverFlags.secure {
		log.Infof("in secure mode")

		m = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(serverFlags.host),
			Cache:      autocert.DirCache(keysdir),
		}

		srv := &http.Server{
			Addr:         ":443",
			Handler:      mux,
			TLSConfig:    &tls.Config{GetCertificate: m.GetCertificate},
			ReadTimeout:  120 * time.Second,
			WriteTimeout: 10 * time.Second,
			ErrorLog:     zap.NewStdLog(log.Desugar()),
		}

		srv.SetKeepAlivesEnabled(false)

		go func() {
			log.Infof("listening on port 443")

			err := srv.ListenAndServeTLS("", "")
			if err != nil {
				log.Fatalw("failed to listen and serve on https", "error", err)
			}
		}()
	}

	var handler http.Handler = mux
	if m != nil {
		// redirect port 80 to 443.
		mux := new(http.ServeMux)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, fmt.Sprintf("https://%s/%s", r.Host, r.RequestURI), http.StatusFound)
		})
		handler = m.HTTPHandler(mux)
	}

	srv := &http.Server{
		Addr:         ":80",
		Handler:      handler,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 10 * time.Second,
		ErrorLog:     zap.NewStdLog(log.Desugar()),
	}

	log.Infof("listening on port 80")

	err := srv.ListenAndServe()
	if err != nil {
		log.Fatalw("failed to listen and serve on http", "error", err)
	}

	return nil
}

func handleUpload(datadir string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		var src = "unknown"
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			src = host
		}

		log := log.With("host", src)
		log.Infof("incoming report")

		// read up to the specified max length.
		reader := http.MaxBytesReader(w, r.Body, serverFlags.maxSize)
		defer reader.Close()

		file, err := ioutil.TempFile("", "")
		if err != nil {
			log.Warnw("failed to create temp file", "error", err)
			http.Error(w, "failed to create temp file", http.StatusInternalServerError)
			return
		}

		n, err := io.Copy(file, reader)
		if err != nil {
			log.Warnw("failed while copying into file", "error", err)
			http.Error(w, "failed while copying into file", http.StatusInternalServerError)
			return
		}

		log.Infow("finished writing temp report", "length", n)

		delete := true
		defer func() {
			if delete {
				log.Infow("deleted temp file", "file", file.Name())
				_ = os.Remove(file.Name())
			}
		}()

		// seek to the beginning of the file.
		_, _ = file.Seek(0, 0)

		var header ReportHeader
		if err := json.NewDecoder(file).Decode(&header); err != nil {
			log.Warnw("could not parse header", "error", err)
			http.Error(w, "invalid header", http.StatusBadRequest)
			return
		}
		if !header.Header {
			log.Warnw("first record is not a header")
			http.Error(w, "invalid header", http.StatusBadRequest)
			return
		}
		switch k := header.Kind; k {
		case "bootstrappers", "miners", "blockpublishers":
		default:
			log.Warnw("unrecognized report kind", "kind", k)
			http.Error(w, "invalid header", http.StatusBadRequest)
			return
		}

		log.Infof("report accepted")

		delete = false

		seq := atomic.AddUint64(&seq, 1)
		final := fmt.Sprintf("%s-%s-%s-%d", header.Kind, src, time.Now().Format(time.RFC3339), seq)
		err = os.Rename(file.Name(), filepath.Join(datadir, final))
		if err != nil {
			log.Infow("failed to move file", "from", file.Name(), "to", final, "error", err)
			return
		}

		log.Infow("report written to final location", "path", final)

		_, _ = w.Write([]byte(final))
	}
}
