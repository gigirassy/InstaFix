package handlers

import (
	"bytes"
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
		results[i] = uint64(costFn(imagesWH, start, i, canvasWidth, 1000))
	}
	return results
}

func avg(n []float64) float64 {
	if len(n) == 0 {
		return 0
	}
	var sum float64
	for _, v := range n {
		sum += v
	}
	return sum / float64(len(n))
}

// GenerateGridFromBytes does the same layout and rendering as the original GenerateGrid,
// but receives JPEG data as compressed bytes and decodes each image one-by-one while rendering.
func GenerateGridFromBytes(imagesData [][]byte) (image.Image, error) {
	// Append the dummy terminal entry (same as original's image.Rect append)
	imagesData = append(imagesData, nil)

	// Build imagesWH from configs (no full decode)
	var imagesWH [][]float64
	imagesWH = make([][]float64, 0, len(imagesData))
	for _, data := range imagesData {
		if data == nil {
			// terminal dummy
			imagesWH = append(imagesWH, []float64{0, 0})
			continue
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		imagesWH = append(imagesWH, []float64{float64(cfg.Width), float64(cfg.Height)})
	}

	// Calculate canvas width by taking the average of width of all images
	var allWidth []float64
	for _, imageWH := range imagesWH {
		allWidth = append(allWidth, imageWH[0])
	}
	canvasWidth := int(avg(allWidth) * 1.5)
	if canvasWidth <= 0 {
		return nil, errors.New("invalid canvas width")
	}

	// Build graph and compute shortest path (same as original)
	graph := dijkstra.NewGraph()
	for i := range imagesWH {
		graph.AddVertexAndArcs(i, createGraph(imagesWH, i, canvasWidth))
	}

	best, err := graph.Shortest(0, len(imagesWH)-1)
	if err != nil {
		return nil, err
	}
	path := best.Path

	// Calculate row heights and canvas height
	canvasHeight := 0
	var heightRows []int
	for i := 1; i < len(path); i++ {
		if len(imagesWH) < path[i-1] {
			return nil, errors.New("imagesWH is not long enough")
		}
		rowWH := imagesWH[path[i-1]:path[i]]
		rowHeight := int(getHeight(rowWH, canvasWidth))
		heightRows = append(heightRows, rowHeight)
		canvasHeight += rowHeight
	}

	// Create the canvas
	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))

	// Render: for each row, decode each image from bytes just in time, scale into canvas, then free bytes.
	oldRowHeight := 0
	for i := 1; i < len(path); i++ {
		inRowStart := path[i-1]
		inRowEnd := path[i]
		oldImWidth := 0
		if len(heightRows) < i {
			return nil, errors.New("heightRows is not long enough")
		}
		heightRow := heightRows[i-1]

		for idx := inRowStart; idx < inRowEnd; idx++ {
			// skip terminal dummy if encountered
			if idx < 0 || idx >= len(imagesData) {
				continue
			}
			data := imagesData[idx]
			if data == nil {
				continue
			}

			img, err := jpeg.Decode(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}

			newWidthF := float64(heightRow) * float64(img.Bounds().Dx()) / float64(img.Bounds().Dy())
			newWidth := int(newWidthF)
			if newWidth <= 0 {
				newWidth = 1
			}

			draw.ApproxBiLinear.Scale(canvas,
				image.Rect(oldImWidth, oldRowHeight, oldImWidth+newWidth, oldRowHeight+heightRow),
				img, img.Bounds(), draw.Src, nil)

			// free decoded image & compressed bytes to reduce peak usage
			img = nil
			imagesData[idx] = nil

			oldImWidth += newWidth
		}
		oldRowHeight += heightRow
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

		// Limit concurrent downloads to avoid spikes (tweak concurrency as needed)
		const maxConcurrentDownloads = 6
		sem := make(chan struct{}, maxConcurrentDownloads)

		dataSlices := make([][]byte, len(mediaURLs))
		errs := make([]error, len(mediaURLs))
		var wg sync.WaitGroup

		for i, url := range mediaURLs {
			wg.Add(1)
			go func(i int, url string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
				if err != nil {
					errs[i] = err
					return
				}

				res, err := client.Do(req)
				if err != nil {
					slog.Error("Failed to get image", "postID", postID, "err", err)
					errs[i] = err
					return
				}
				defer res.Body.Close()

				// Read compressed bytes (smaller than decoded image)
				b, err := io.ReadAll(res.Body)
				if err != nil {
					errs[i] = err
					return
				}
				dataSlices[i] = b
			}(i, url)
		}

		wg.Wait()

		// On any download error, cleanup and return error
		for _, e := range errs {
			if e != nil {
				for _, b := range dataSlices {
					if b != nil {
						// allow GC by nil-ing (no files on disk used)
						b = nil
					}
				}
				return false, e
			}
		}

		// Build grid image from compressed bytes (decodes one-by-one while rendering)
		grid, err := GenerateGridFromBytes(dataSlices)
		if err != nil {
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
