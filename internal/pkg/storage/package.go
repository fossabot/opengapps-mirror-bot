package storage

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nezorflame/opengapps-mirror-bot/pkg/gapps"
	"github.com/nezorflame/opengapps-mirror-bot/pkg/net"

	"github.com/google/go-github/v25/github"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

const gappsSeparator = "-"

// Package describes the OpenGApps package
type Package struct {
	Name      string         `json:"name"`
	Date      string         `json:"date"`
	OriginURL string         `json:"origin_url"`
	LocalURL  string         `json:"local_url"`
	RemoteURL string         `json:"remote_url"`
	MD5       string         `json:"md5"`
	Size      int            `json:"size"`
	Platform  gapps.Platform `json:"platform"`
	Android   gapps.Android  `json:"android"`
	Variant   gapps.Variant  `json:"variant"`
}

// CreateMirror creates a new mirror for the package
func (p *Package) CreateMirror(dq *net.DownloadQueue, cfg *viper.Viper) error {
	if cfg.GetString("gapps.local_url") != "" && p.LocalURL != "" ||
		cfg.GetString("gapps.remote_url") != "" && p.RemoteURL != "" {
		return nil
	}

	// download the file
	filePath, err := dq.AddMultiple(p.OriginURL, p.MD5, 20, p.Size)
	if err != nil {
		return errors.Wrap(err, "unable to read file body")
	}
	log.Debugf("Package downloaded to %s", filePath)

	// if we have local_path set, save the file there
	if localPath := cfg.GetString("gapps.local_path"); localPath != "" {
		if filePath, err = p.move(filePath, localPath); err != nil {
			return errors.Wrap(err, "unable to move the file to storage")
		}
		log.Debugf("Package moved to %s", filePath)

		// if we have local_url set, provide the local server URL
		if localURL := cfg.GetString("gapps.local_url"); localURL != "" {
			relPath := strings.TrimPrefix(filePath, localPath)
			p.LocalURL = fmt.Sprintf(localURL, relPath)
			log.Debugf("Local URL is %s", p.LocalURL)
		}
	} else {
		// delete the file in the end otherwise
		log.Debug("Temp file will be deleted")
		defer os.Remove(filePath)
	}

	// if we have remote_url set, send the file to remote URL
	if remoteURL := cfg.GetString("gapps.remote_url"); remoteURL != "" {
		tmpFile, err := os.Open(filepath.Clean(filePath))
		if err != nil {
			return errors.Wrap(err, "unable to create temp file")
		}
		defer tmpFile.Close()

		req, err := http.NewRequest(http.MethodPut, fmt.Sprintf(remoteURL, p.Name), tmpFile)
		if err != nil {
			return errors.Wrap(err, "unable to create upload request")
		}
		req.Header.Set("Content-Type", "application/zip")
		req.Header.Set("Max-Days", "7")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return errors.Wrap(err, "unable to make upload request")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return errors.Errorf("unable to make upload request: %v", resp.Status)
		}

		result, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return errors.Wrap(err, "unable to read mirror response body")
		}

		p.RemoteURL = string(result)
		log.Debugf("File uploaded, remote URL is %s", p.RemoteURL)
	}

	return nil
}

func (p *Package) move(origin, destFolder string) (string, error) {
	path := destFolder + p.Platform.String() + "/" + p.Date

	//nolint:gosec
	if err := os.MkdirAll(path, 0755); err != nil {
		return "", errors.Wrap(err, "unable to create folder")
	}

	path += "/" + p.Name
	if err := os.Rename(origin, path); err != nil {
		return "", errors.Wrap(err, "unable to move file")
	}

	//nolint:gosec
	if err := os.Chmod(path, 0755); err != nil {
		return "", errors.Wrap(err, "unable to set file permissions")
	}

	return path, nil
}

func formPackage(dq *net.DownloadQueue, cfg *viper.Viper, zipAsset, md5Asset github.ReleaseAsset) (*Package, error) {
	md5sum, err := getMD5(dq, md5Asset.GetBrowserDownloadURL())
	if err != nil {
		return nil, errors.Wrap(err, "unable to download md5")
	}

	p, err := parseAsset(cfg, zipAsset, md5sum)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create package")
	}

	return p, nil
}

func getMD5(dq *net.DownloadQueue, url string) (string, error) {
	filePath, err := dq.AddSingle(url)
	if err != nil {
		return "", errors.Wrap(err, "unable to download MD5 file")
	}

	file, err := os.Open(filepath.Clean(filePath))
	if err != nil {
		return "", errors.Wrap(err, "unable to open MD5 file")
	}
	defer file.Close()

	result, err := ioutil.ReadAll(file)
	if err != nil {
		return "", errors.Wrap(err, "unable to read MD5 file")
	}

	return strings.Split(string(result), "  ")[0], nil
}

// Package name format is as follows:
// open_gapps-Platform-Android-Variant-Date.zip
func parseAsset(cfg *viper.Viper, asset github.ReleaseAsset, md5Sum string) (*Package, error) {
	name := asset.GetName()
	parts := strings.Split(strings.TrimPrefix(name, cfg.GetString("gapps.prefix")+gappsSeparator), ".")
	if len(parts) != 3 {
		return nil, errors.Errorf("incorrect package name: %s", name)
	}

	path, ext := parts[0]+parts[1], parts[2]
	if ext != "zip" {
		return nil, errors.Errorf("incorrect package extension: %s", ext)
	}

	parts = strings.Split(path, gappsSeparator)
	if len(parts) != 4 {
		return nil, errors.Errorf("incorrect package name: %s", name)
	}

	platform, android, variant, err := gapps.ParsePackageParts(parts[:3])
	if err != nil {
		return nil, err
	}

	if _, err = time.Parse(cfg.GetString("gapps.time_format"), parts[3]); err != nil {
		return nil, errors.Wrap(err, "unable to parse time")
	}

	return &Package{
		Name:      name,
		Date:      parts[3],
		OriginURL: asset.GetBrowserDownloadURL(),
		MD5:       md5Sum,
		Size:      asset.GetSize(),
		Platform:  platform,
		Android:   android,
		Variant:   variant,
	}, nil
}