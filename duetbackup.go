package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	sysDir          = "0:/sys"
	typeDirectory   = "d"
	typeFile        = "f"
	fileDownloadURL = "/rr_download?name="
	fileListURL     = "/rr_filelist?dir="
	dirMarker       = ".duetbackup"
)

var multiSlashRegex = regexp.MustCompile(`/{2,}`)
var httpClient *http.Client

type localTime struct {
	Time time.Time
}

// file resembles the JSON object returned in the files property of the rr_filelist response
type file struct {
	Type string
	Name string
	Size uint64
	Date localTime
}

// filelist resembled the JSON object in rr_filelist
type filelist struct {
	Dir   string
	Files []file
	next  uint64
}

func (lt *localTime) UnmarshalJSON(b []byte) (err error) {
	// Parse date string in local time (it does not provide any timezone information)
	lt.Time, err = time.ParseInLocation(`"2006-01-02T15:04:05"`, string(b), time.Local)
	return err
}

type excludes struct {
	excls []string
}

func (e *excludes) String() string {
	return strings.Join(e.excls, ",")
}

func (e *excludes) Set(value string) error {
	e.excls = append(e.excls, cleanPath(value))
	return nil
}

// Contains checks if the given path starts with any of the known excludes
func (e *excludes) Contains(path string) bool {
	for _, excl := range e.excls {
		if strings.HasPrefix(path, excl) {
			return true
		}
	}
	return false
}

// cleanPath will reduce multiple consecutive slashes into one and
// then remove a trailing slash if any.
func cleanPath(path string) string {
	cleanedPath := multiSlashRegex.ReplaceAllString(path, "/")
	cleanedPath = strings.TrimSuffix(cleanedPath, "/")
	return cleanedPath
}

// download will perform a GET request on the given URL and return
// the content of the response, a duration on how long it took (including
// setup of connection) or an error in case something went wrong
func download(url string) ([]byte, *time.Duration, error) {
	start := time.Now()
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	duration := time.Since(start)
	if err != nil {
		return nil, nil, err
	}
	return body, &duration, nil
}

func getFileList(baseURL string, dir string, first uint64) (*filelist, error) {

	body, _, err := download(baseURL + fileListURL + dir)
	if err != nil {
		return nil, err
	}

	var fl filelist
	err = json.Unmarshal(body, &fl)
	if err != nil {
		return nil, err
	}

	// If the response signals there is more to fetch do it recursively
	if fl.next > 0 {
		moreFiles, err := getFileList(baseURL, dir, fl.next)
		if err != nil {
			return nil, err
		}
		fl.Files = append(fl.Files, moreFiles.Files...)
	}

	// Sort folders first and by name
	sort.SliceStable(fl.Files, func(i, j int) bool {

		// Both same type so compare by name
		if fl.Files[i].Type == fl.Files[j].Type {
			return fl.Files[i].Name < fl.Files[j].Name
		}

		// Different types -> sort folders first
		return fl.Files[i].Type == typeDirectory
	})
	return &fl, nil
}

// ensureOutDirExists will create the local directory if it does not exist
// and will in any case create the marker file inside it
func ensureOutDirExists(outDir string, verbose bool) error {
	path, err := filepath.Abs(outDir)
	if err != nil {
		return err
	}

	// Check if the directory exists
	fi, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	// Create the directory
	if fi == nil {
		if verbose {
			log.Println("  Creating directory", path)
		}
		if err = os.MkdirAll(path, 0755); err != nil {
			return err
		}
	}

	// Create the marker file
	markerFile, err := os.Create(filepath.Join(path, dirMarker))
	if err != nil {
		return err
	}
	markerFile.Close()

	return nil
}

func updateLocalFiles(baseURL string, fl *filelist, outDir string, excls excludes, removeLocal, verbose bool) error {

	if err := ensureOutDirExists(outDir, verbose); err != nil {
		return err
	}

	for _, file := range fl.Files {
		if file.Type == typeDirectory {
			continue
		}
		remoteFilename := fl.Dir + "/" + file.Name

		// Skip files covered by an exclude pattern
		if excls.Contains(remoteFilename) {
			if verbose {
				log.Println("  Excluding: ", remoteFilename)
			}
			continue
		}

		fileName := filepath.Join(outDir, file.Name)
		fi, err := os.Stat(fileName)
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		// File does not exist or is outdated so get it
		if fi == nil || fi.ModTime().Before(file.Date.Time) {
			if verbose {
			}

			// Download file
			body, duration, err := download(baseURL + fileDownloadURL + url.QueryEscape(remoteFilename))
			if err != nil {
				return err
			}
			if verbose {
				kibs := (float64(file.Size) / duration.Seconds()) / 1024
				if fi != nil {
					log.Printf("  Updated:   %s (%.1f KiB/s)", remoteFilename, kibs)
				} else {
					log.Printf("  Added:     %s (%.1f KiB/s)", remoteFilename, kibs)
				}
			}

			// Open or create corresponding local file
			nf, err := os.Create(fileName)
			if err != nil {
				return err
			}
			defer nf.Close()

			// Write contents to local file
			_, err = nf.Write(body)
			if err != nil {
				return err
			}

			// Adjust mtime
			os.Chtimes(fileName, file.Date.Time, file.Date.Time)
		} else {
			if verbose {
				log.Println("  Up-to-date:", remoteFilename)
			}
		}

	}

	return nil
}

// isManagedDirectory checks wether the given path is a directory and
// if so if it contains the marker file. It will return false in case
// any error has occured.
func isManagedDirectory(basePath string, f os.FileInfo) bool {
	if !f.IsDir() {
		return false
	}
	markerFile := filepath.Join(basePath, f.Name(), dirMarker)
	fi, err := os.Stat(markerFile)
	if err != nil && !os.IsNotExist(err) {
		return false
	}
	if fi == nil {
		return false
	}
	return true
}

func removeDeletedFiles(fl *filelist, outDir string, verbose bool) error {

	// Pseudo hash-set of known remote filenames
	existingFiles := make(map[string]struct{})
	for _, f := range fl.Files {
		existingFiles[f.Name] = struct{}{}
	}

	files, err := ioutil.ReadDir(outDir)
	if err != nil {
		return err
	}

	for _, f := range files {
		if _, exists := existingFiles[f.Name()]; !exists {

			// Skip directories not managed by us as well as our marker file
			if !isManagedDirectory(outDir, f) || f.Name() == dirMarker {
				continue
			}
			if err := os.RemoveAll(filepath.Join(outDir, f.Name())); err != nil {
				return err
			}
			if verbose {
				log.Println("  Removed:   ", f.Name())
			}
		}
	}

	return nil
}

func syncFolder(address, folder, outDir string, excls excludes, removeLocal, verbose bool) error {

	// Skip complete directories if they are covered by an exclude pattern
	if excls.Contains(folder) {
		log.Println("Excluding", folder)
		return nil
	}

	log.Println("Fetching filelist for", folder)
	fl, err := getFileList(address, url.QueryEscape(folder), 0)
	if err != nil {
		return err
	}

	log.Println("Downloading new/changed files from", folder, "to", outDir)
	if err = updateLocalFiles(address, fl, outDir, excls, removeLocal, verbose); err != nil {
		return err
	}

	if removeLocal {
		log.Println("Removing no longer existing files in", outDir)
		if err = removeDeletedFiles(fl, outDir, verbose); err != nil {
			return err
		}
	}

	// Traverse into subdirectories
	for _, file := range fl.Files {
		if file.Type != typeDirectory {
			continue
		}
		remoteFilename := fl.Dir + "/" + file.Name
		fileName := filepath.Join(outDir, file.Name)
		if err = syncFolder(address, remoteFilename, fileName, excls, removeLocal, verbose); err != nil {
			return err
		}
	}

	return nil
}

func getAddress(domain string, port uint64) string {
	return "http://" + domain + ":" + strconv.FormatUint(port, 10)
}

func connect(address, password string, verbose bool) error {
	if verbose {
		log.Println("Trying to connect to Duet")
	}
	path := "/rr_connect?password=" + url.QueryEscape(password) + "&time=" + url.QueryEscape(time.Now().Format("2006-01-02T15:04:05"))
	_, err := httpClient.Get(address + path)
	return err
}

func main() {
	var domain, dirToBackup, outDir, password string
	var removeLocal, verbose bool
	var port uint64
	var excls excludes

	flag.StringVar(&domain, "domain", "", "Domain of Duet Wifi")
	flag.Uint64Var(&port, "port", 80, "Port of Duet Wifi")
	flag.StringVar(&dirToBackup, "dirToBackup", sysDir, "Directory on Duet to create a backup of")
	flag.StringVar(&outDir, "outDir", "", "Output dir of backup")
	flag.StringVar(&password, "password", "reprap", "Connection password")
	flag.BoolVar(&removeLocal, "removeLocal", false, "Remove files locally that have been deleted on the Duet")
	flag.BoolVar(&verbose, "verbose", false, "Output more details")
	flag.Var(&excls, "exclude", "Exclude paths starting with this string (can be passed multiple times)")
	flag.Parse()

	if domain == "" || outDir == "" {
		log.Fatal("-domain and -outDir are mandatory parameters")
	}

	if port > 65535 {
		log.Fatal("Invalid port", port)
	}

	tr := &http.Transport{DisableCompression: true}
	httpClient = &http.Client{Transport: tr}

	address := getAddress(domain, port)

	// Try to connect
	if err := connect(address, password, verbose); err != nil {
		log.Println("Duet currently not available")
		os.Exit(0)
	}

	// Get absolute path from user's input
	absPath, err := filepath.Abs(outDir)
	if err != nil {
		// Fall back to original user's input
		absPath = outDir
	}

	if err = syncFolder(address, cleanPath(dirToBackup), absPath, excls, removeLocal, verbose); err != nil {
		log.Fatal(err)
	}
}
