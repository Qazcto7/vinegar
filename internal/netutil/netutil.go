// Package netutil provides shared utility networking functions.
package netutil

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gtk"
)

type progressCounter struct {
	total   uint64
	current uint64
	pbar    *gtk.ProgressBar
}

func (pc *progressCounter) Write(p []byte) (int, error) {
	n := len(p)
	pc.current += uint64(n)
	return n, nil
}

// ErrBadStatus is the error returned by Download and Body
// if the returned HTTP status code is not http.StatusOK.
var ErrBadStatus = errors.New("bad status")

// DownloadProgress downloads the named url to the named file, using
// df as the callback for progress. No retry will be checked here.
func DownloadProgress(url, file string, pbar *gtk.ProgressBar) error {
	out, err := os.Create(file)
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", ErrBadStatus, resp.Status)
	}

	// A missing/unknown Content-Length is reported as -1. Converting
	// that straight to uint64 used to wrap around to ~1.8e19, which
	// silently broke two things at once: the progress fraction stayed
	// at effectively 0% for the whole download, and the timeout below
	// only ever stopped itself once pc.current reached that huge
	// pc.total - i.e. never - leaking a 16ms GLib timer for the rest of
	// the process's lifetime. Fall back to a pulsing (indeterminate)
	// bar when the length isn't known.
	if resp.ContentLength <= 0 {
		var pulsecb glib.SourceFunc = func(uintptr) bool {
			pbar.Pulse()
			return true
		}
		id := glib.TimeoutAdd(128, &pulsecb, 0)
		defer glib.SourceRemove(id)

		_, err = io.Copy(out, resp.Body)
		return err
	}

	pc := &progressCounter{
		total: uint64(resp.ContentLength),
		pbar:  pbar,
	}

	var idlecb glib.SourceFunc = func(uintptr) bool {
		pbar.SetFraction(float64(pc.current) / float64(pc.total))
		return pc.current != pc.total
	}
	id := glib.TimeoutAdd(16, &idlecb, 0)
	defer glib.SourceRemove(id)

	_, err = io.Copy(out, io.TeeReader(resp.Body, pc))
	return err
}

// Download downloads the named url to the named file. If an error
// occurs when downloading the file. Download will retry 3 times before
// returning a final error.
func Download(url, file string) error {
	retries := 3
	for i := 0; i < retries; i++ {
		err := download(url, file)
		if err == nil {
			break
		}

		// additional condition for if the error was a file error or status error
		if _, ok := err.(*os.PathError); err != nil &&
			(i == retries-1 || ok || errors.Is(err, ErrBadStatus)) {
			os.Remove(file) // just remove the thing anyway on failure
			return err
		}

		log.Printf("Download %s failed, retrying...", url)
	}

	return nil
}

func download(url, file string) error {
	out, err := os.Create(file)
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", ErrBadStatus, resp.Status)
	}

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	return nil
}
