package main

import (
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/russross/smugmug"
)

var (
	apiKey   string
	email    string
	password string
	dir      string
	dry      bool
	del      bool
	fast     bool
	jobs     int
	videos   bool
	pics     bool

	fileCount  int
	totalBytes int
)

func main() {
	start := time.Now()

	// parse config
	configString(&apiKey, "apikey", "", "SmugMug API key")
	configString(&email, "email", "", "Email address")
	configString(&password, "password", "", "Password")
	configString(&dir, "dir", "", "Target directory")
	flag.BoolVar(&dry, "dry", false, "Dry run (no changes)")
	flag.BoolVar(&del, "delete", true, "Delete local files not in album")
	flag.BoolVar(&fast, "fast", true, "Skip albums with timestamp match")
	flag.BoolVar(&videos, "videos", true, "Download videos")
	flag.BoolVar(&pics, "pics", true, "Download pictures")
	flag.IntVar(&jobs, "jobs", 1, "Number of concurrent jobs to run")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("Unknown command-line options: %s", strings.Join(flag.Args(), " "))
	}
	if apiKey == "" || email == "" || password == "" {
		log.Fatalf("apikey, email, and password are all required")
	}
	if dir == "" {
		dir = "."
	}
	d, err := filepath.Abs(dir)
	if err != nil {
		log.Fatalf("Unable to find absolute path for %s: %v", dir, err)
	}
	dir = d

	// login
	c, err := smugmug.Login(email, password, apiKey)
	if err != nil {
		log.Fatalf("Login error: %v", err)
	}
	log.Printf("Logged in %s, NickName is %s", email, c.NickName)

	// get full list of albums
	albums, err := c.Albums(c.NickName)
	if err != nil {
		log.Fatalf("Albums error: %v", err)
	}
	log.Printf("Found %d albums", len(albums))

	// process each album
	rate := make(chan struct{}, jobs)
	for _, album := range albums {
		rate <- struct{}{}
		go func(album *smugmug.AlbumInfo) {
			if err := processAlbum(c, album); err != nil {
				log.Fatalf("Error processing album %s: %v", album.URL, err)
			}
			<-rate
		}(album)
	}

	// wait for remaining jobs to finish
	for i := 0; i < jobs; i++ {
		rate <- struct{}{}
	}

	if totalBytes > 1024*1024 {
		log.Printf("Downloaded %d files (%.1fm) in %v", fileCount, float64(totalBytes)/(1024*1024), time.Since(start))
	} else if totalBytes > 1024 {
		log.Printf("Downloaded %d files (%.1fk) in %v", fileCount, float64(totalBytes)/1024, time.Since(start))
	} else {
		log.Printf("Downloaded %d files (%d bytes) in %v", fileCount, totalBytes, time.Since(start))
	}
}

func processAlbum(c *smugmug.Conn, album *smugmug.AlbumInfo) error {
	path := album.Category.Name
	if album.SubCategory != nil {
		path = filepath.Join(path, album.SubCategory.Name)
	}
	path = filepath.Join(path, album.Title)
	fullpath := filepath.Join(dir, path)
	updated, err := time.ParseInLocation("2006-01-02 15:04:05", album.LastUpdated, time.Local)
	if err != nil {
		return fmt.Errorf("Unable to parse timestamp %q: %v", album.LastUpdated, err)
	}

	// see if we can skip this based on a time stamp
	if fast {
		info, err := os.Stat(fullpath)
		if err == nil && info.IsDir() && info.ModTime().Equal(updated) {
			log.Printf("Skipping %s [%s], timestamp of %s matches", path, album.URL, album.LastUpdated)
			return nil
		}
	}

	log.Printf("Processing %s [%s] (updated %s)", path, album.URL, album.LastUpdated)

	// scan the local directory: map path to md5sum
	localFiles := make(map[string]string)
	if info, err := os.Stat(fullpath); err == nil && info.IsDir() {
		if err := filepath.Walk(fullpath, filepath.WalkFunc(func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			suffix := path
			if strings.HasPrefix(path, dir+"/") {
				suffix = path[len(dir)+1:]
			}

			if info.IsDir() {
				localFiles[suffix] = "directory"
				return nil
			}

			// get an MD5 hash
			h := md5.New()
			f, err := os.Open(path)
			if err != nil {
				log.Printf("error opening %s: %v", path, err)
				return err
			}
			defer f.Close()
			if _, err = io.Copy(h, f); err != nil {
				log.Printf("error reading %s: %v", path, err)
				return err
			}
			sum := h.Sum(nil)
			s := hex.EncodeToString(sum)
			localFiles[suffix] = s
			return nil
		})); err != nil && err != os.ErrNotExist {
			return fmt.Errorf("error walking local file system: %v", err)
		}
	}

	// get full list of images from this album
	images, err := c.Images(album)
	if err != nil {
		return fmt.Errorf("Images error: %v", err)
	}

	// process each image
	for _, img := range images {
		if err := syncFile(album, img, localFiles, dir); err != nil {
			return fmt.Errorf("Error processing image %s from album %s in category %s: %v",
				img.FileName, album.Title, album.Category.Name, err)
		}
	}

	// delete extra files
	if err = cleanup(localFiles, dir); err != nil {
		return fmt.Errorf("Error cleaning up: %v", err)
	}

	// update the directory timestamp to match
	if !dry {
		if err = os.Chtimes(fullpath, updated, updated); err != nil {
			return fmt.Errorf("failed to set timestamp on directory %s: %v", fullpath, err)
		}
	}

	return nil
}

func syncFile(album *smugmug.AlbumInfo, image *smugmug.ImageInfo, localFiles map[string]string, dir string) error {
	path := album.Category.Name
	if album.SubCategory != nil {
		path = filepath.Join(path, album.SubCategory.Name)
	}
	path = filepath.Join(path, album.Title)
	if image.FileName != "" {
		path = filepath.Join(path, image.FileName)
	} else {
		return fmt.Errorf("image with no filename: ID=%d Key=%s Album=%v", image.ID, image.Key, image.Album)
	}

	// skip based on type of file
	if isVideo(image.Format) && !videos {
		log.Printf("    skipping video file %s", path)
		// mark this local file as existing on the server
		delete(localFiles, path)
		delete(localFiles, filepath.Dir(path))

		return nil
	} else if !isVideo(image.Format) && !pics {
		log.Printf("    skipping picture file %s", path)
		// mark this local file as existing on the server
		delete(localFiles, path)
		delete(localFiles, filepath.Dir(path))

		return nil
	}

	if localFiles[path] == image.MD5Sum {
		log.Printf("    skipping unchanged file %s", path)

		// mark this local file as existing on the server
		delete(localFiles, path)
		delete(localFiles, filepath.Dir(path))

		return nil
	}

	if localFiles[path] != "" && isVideo(image.Format) {
		log.Printf("    skipping existing video (assuming unchanged) %s", path)

		// mark this local file as existing on the server
		delete(localFiles, path)
		delete(localFiles, filepath.Dir(path))

		return nil
	}

	// file is new/changed, so download it
	fullpath := filepath.Join(dir, path)

	changed := "(new file)"
	if localFiles[path] != "" {
		changed = "(file changed)"
	}

	// mark this local file as existing on the server
	delete(localFiles, path)
	delete(localFiles, filepath.Dir(path))

	if dry {
		log.Printf("    %s: dry run, no downloading %s", path, changed)
		totalBytes += image.Size
		fileCount++
		return nil
	}

	url := image.OriginalURL
	if isVideo(image.Format) {
		if image.Video1920URL != "" {
			url = image.Video1920URL
		} else if image.Video1280URL != "" {
			url = image.Video1280URL
		} else if image.Video960URL != "" {
			url = image.Video960URL
		} else if image.Video640URL != "" {
			url = image.Video640URL
		} else if image.Video320URL != "" {
			url = image.Video320URL
		} else {
			return fmt.Errorf("no valid url found for video")
		}
	}
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("error downloading %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code downloading %s: %d", url, resp.StatusCode)
	}

	// create the directory if necessary
	if err = os.MkdirAll(filepath.Dir(fullpath), 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %v", filepath.Dir(fullpath), err)
	}
	fp, err := os.Create(fullpath)
	if err != nil {
		return fmt.Errorf("failed to open %s for writing: %v", fullpath, err)
	}
	defer fp.Close()
	size, err := io.Copy(fp, resp.Body)
	if err != nil {
		return fmt.Errorf("error saving file %s: %v", fullpath, err)
	}
	if int(size) != image.Size && !isVideo(image.Format) {
		return fmt.Errorf("downloaded %d bytes from %s, expected %d", size, url, image.Size)
	}
	if size > 1024*1024 {
		log.Printf("    %s: downloaded %.1fm %s", path, float64(size)/(1024*1024), changed)
	} else if size > 1024 {
		log.Printf("    %s: downloaded %.1fk %s", path, float64(size)/1024, changed)
	} else {
		log.Printf("    %s: downloaded %d bytes %s", path, size, changed)
	}
	totalBytes += int(size)
	fileCount++

	return nil
}

func cleanup(localFiles map[string]string, dir string) error {
	if !del {
		return nil
	}

	// delete local file not found on server
	for k, v := range localFiles {
		if v == "directory" {
			continue
		}
		if dry {
			log.Printf("dry run, not removing file %s", k)
		} else {
			fullpath := filepath.Join(dir, k)
			if err := os.Remove(fullpath); err != nil {
				return fmt.Errorf("error removing file %s: %v", fullpath, err)
			}
		}
	}

	// delete directories found but not used
	for k, v := range localFiles {
		if v != "directory" {
			continue
		}
		if dry {
			log.Printf("dry run, not removing directory %s", k)
		} else {
			fullpath := filepath.Join(dir, k)
			if err := os.Remove(fullpath); err != nil {
				return fmt.Errorf("error removing directory %s: %v", fullpath, err)
			}
		}
	}

	if len(localFiles) > 0 {
		log.Printf("removed %d files and directories", len(localFiles))
	}

	return nil
}

// configString sets a config variable with a string value
// in ascending priority:
// 1. Default value passed in
// 2. Environment variable value (name in upper case)
// 3. Command-line argument (parameters mimic flag.StringVar)
func configString(p *string, name, value, usage string) {
	if s := os.Getenv(strings.ToUpper(name)); s != "" {
		// set it to environment value if available
		*p = s
	} else {
		// fall back to default
		*p = value
	}

	// pass it on to flag
	flag.StringVar(p, name, *p, usage)
}

func isVideo(format string) bool {
	switch format {
	case "MP4", "AVI":
		return true
	case "JPG", "PNG", "GIF":
		return false
	default:
		log.Fatalf("unknown image format: %s", format)
	}
	return false
}
