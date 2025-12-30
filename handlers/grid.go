package handlers

import (
	"errors"
	"image"
	"image/jpeg"
	scraper "instafix/handlers/scraper"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RyanCarrier/dijkstra/v2"
	"github.com/go-chi/chi/v5"
	"golang.org/x/image/draw"
	"golang.org/x/sync/singleflight"
)

var timeout = 60 * time.Second
var transport = &http.Transport{
	Proxy: nil, // Skip any proxy
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}
var sflightGrid singleflight.Group

// getHeight returns the height of the rows, imagesWH [w,h]
func getHeight(imagesWH [][]float64, canvasWidth int) float64 {
	var height float64
	for _, im := range imagesWH {
		// accumulate w/h
		height += im[0] / im[1]
	}
	return float64(canvasWidth) / height
}

// costFn returns the cost of the row graph thingy
func costFn(imagesWH [][]float64, i, j, canvasWidth, maxRowHeight int) float64 {
	slices := imagesWH[i:j]
	rowHeight := getHeight(slices, canvasWidth)
	return math.Pow(float64(maxRowHeight)-rowHeight, 2)
}

func createGraph(imagesWH [][]float64, start, canvasWidth int) map[int]uint64 {
	results := make(map[int]uint64, len(imagesWH))
	results[start] = 0
	for i := start + 1; i < len(imagesWH); i++ {
		// Max 3 images for every row
		if i-start > 3 {
			break
		}
		c := costFn(imagesWH, start, i, canvasWidth, 1000)
		if c < 0 {
			c = 0
		}
		results[i] = uint64(c)
	}
	return results
}

func avg(n []float64) float64 {
	var sum float64
	for _, v := range n {
		sum += v
	}
	if len(n) == 0 {
		return 0
	}
	return sum / float64(len(n))
}

// computeLayout computes layout (path, heightRows, canvasWidth, canvasHeight) using only widths/heights (no full images).
func computeLayout(imagesWH [][]float64) (path []int, heightRows []int, canvasWidth int, canvasHeight int, err error) {
	// Calculate canvas width by taking the average of width of all images (approximately)
	var allWidth []float64
	allWidth = make([]float64, 0, len(imagesWH))
	for _, im := range imagesWH {
		allWidth = append(allWidth, im[0])
	}
	canvasWidth = int(avg(allWidth) * 1.5)
	if canvasWidth <= 0 {
		return nil, nil, 0, 0, errors.New("invalid canvas width")
	}

	graph := dijkstra.NewGraph()
	for i := range imagesWH {
		graph.AddVertexAndArcs(i, createGraph(imagesWH, i, canvasWidth))
	}

	// shortest path from 0 to len(imagesWH)-1
	best, err := graph.Shortest(0, len(imagesWH)-1)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	path = best.Path

	canvasHeight = 0
	heightRows = make([]int, 0, len(path)-1)
	for i := 1; i < len(path); i++ {
		if len(imagesWH) < path[i-1] {
			return nil, nil, 0, 0, errors.New("imagesWH is not long enough")
		}
		rowWH := imagesWH[path[i-1]:path[i]]
		rowHeight := int(getHeight(rowWH, canvasWidth))
		heightRows = append(heightRows, rowHeight)
		canvasHeight += rowHeight
	}
	return path, heightRows, canvasWidth, canvasHeight, nil
}

// renderGrid renders the canvas by decoding each image file one at a time to keep memory usage low.
func renderGrid(tempFiles []string, path []int, heightRows []int, canvasWidth, canvasHeight int) (image.Image, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))

	oldRowHeight := 0
	for rowIndex := 1; rowIndex < len(path); rowIndex++ {
		start := path[rowIndex-1]
		end := path[rowIndex]
		if rowIndex-1 >= len(heightRows) {
			return nil, errors.New("heightRows is not long enough")
		}
		heightRow := heightRows[rowIndex-1]
		oldImWidth := 0

		for idx := start; idx < end; idx++ {
			// Open temp file and decode just this image
			tf := tempFiles[idx]
			f, err := os.Open(tf)
			if err != nil {
				return nil, err
			}
			img, err := jpeg.Decode(f)
			f.Close()
			if err != nil {
				return nil, err
			}

			newWidthF := float64(heightRow) * float64(img.Bounds().Dx()) / float64(img.Bounds().Dy())
			newWidth := int(newWidthF)

			// draw scaled image into canvas
			dstRect := image.Rect(oldImWidth, oldRowHeight, oldImWidth+newWidth, oldRowHeight+heightRow)
			draw.ApproxBiLinear.Scale(canvas, dstRect, img, img.Bounds(), draw.Src, nil)

			// free img variable (help GC)
			img = nil

			oldImWidth += newWidth

			// remove temp file to free disk
			_ = os.Remove(tf)
		}

		oldRowHeight += heightRow
	}
	return canvas, nil
}

// GenerateGrid now accepts only image dimension metadata and returns the canvas image after rendering images from files.
func GenerateGridFromFiles(tempFiles []string) (image.Image, error) {
	// Build imagesWH from jpeg configs (we rely on caller having created temp files and filled them)
	imagesWH := make([][]float64, 0, len(tempFiles))
	for _, tf := range tempFiles {
		f, err := os.Open(tf)
		if err != nil {
			return nil, err
		}
		cfg, err := jpeg.DecodeConfig(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		imagesWH = append(imagesWH, []float64{float64(cfg.Width), float64(cfg.Height)})
	}

	// compute layout
	path, heightRows, canvasWidth, canvasHeight, err := computeLayout(imagesWH)
	if err != nil {
		return nil, err
	}

	// render images one-by-one from tempFiles
	canvas, err := renderGrid(tempFiles, path, heightRows, canvasWidth, canvasHeight)
	if err != nil {
		return nil, err
	}
	return canvas, nil
}

func Grid(w http.ResponseWriter, r *http.Request) {
	postID := chi.URLParam(r, "postID")
	gridFname := filepath.Join("static", postID+".jpeg")

	// If already exists, return from cache
	if _, ok := scraper.LRU.Get(gridFname); ok {
		f, err := os.Open(gridFname)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "image/jpeg")
			io.Copy(w, f)
			return
		}
	}

	item, err := scraper.GetData(postID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Filter media only include image
	var mediaURLs []string
	for _, media := range item.Medias {
		if !strings.Contains(media.TypeName, "Image") {
			continue
		}
		mediaURLs = append(mediaURLs, media.URL)
	}

	if len(item.Medias) == 1 || len(mediaURLs) == 1 {
		http.Redirect(w, r, "/images/"+postID+"/1", http.StatusFound)
		return
	}

	_, err, _ = sflightGrid.Do(postID, func() (interface{}, error) {
		client := http.Client{Transport: transport, Timeout: timeout}

		// Download each image to a temp file (concurrently). We do not decode them into memory.
		tempFiles := make([]string, len(mediaURLs))
		errs := make([]error, len(mediaURLs))
		var wg sync.WaitGroup

		for i, mediaURL := range mediaURLs {
			wg.Add(1)
			go func(i int, url string) {
				defer wg.Done()
				req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
				if err != nil {
					errs[i] = err
					return
				}
				res, err := client.Do(req)
				if err != nil {
					slog.Error("Failed to GET image", "postID", postID, "err", err, "url", url)
					errs[i] = err
					return
				}
				defer res.Body.Close()

				// create temp file
				tf, err := os.CreateTemp("", "gridimg_*")
				if err != nil {
					errs[i] = err
					return
				}
				// copy stream to file (no large in-memory buffer)
				_, err = io.Copy(tf, res.Body)
				if err != nil {
					tf.Close()
					_ = os.Remove(tf.Name())
					errs[i] = err
					return
				}
				tf.Close()
				tempFiles[i] = tf.Name()
			}(i, mediaURL)
		}
		wg.Wait()

		// Check for download errors and cleanup on error
		for _, e := range errs {
			if e != nil {
				for _, tf := range tempFiles {
					if tf != "" {
						_ = os.Remove(tf)
					}
				}
				return false, e
			}
		}

		// Create grid Images from temp files (uses only configs first, decodes full images one by one)
		grid, err := GenerateGridFromFiles(tempFiles)
		if err != nil {
			// cleanup temp files on error
			for _, tf := range tempFiles {
				if tf != "" {
					_ = os.Remove(tf)
				}
			}
			return false, err
		}

		// Write grid to static folder
		f, err := os.Create(gridFname)
		if err != nil {
			return false, err
		}
		defer f.Close()

		if err := jpeg.Encode(f, grid, &jpeg.Options{Quality: 80}); err != nil {
			return false, err
		}
		scraper.LRU.Add(gridFname, true)
		return true, nil
	})

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	f, err := os.Open(gridFname)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	io.Copy(w, f)
}
