package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"code.gitea.io/git"
	cm "github.com/honestbee/drone-chartmuseum/pkg/cmclient"
	"github.com/honestbee/drone-chartmuseum/pkg/util"
	"github.com/pkg/errors"
	"k8s.io/helm/pkg/chartutil"
	"k8s.io/helm/pkg/ignore"
)

type (

	// Config struct map with drone plugin parameters
	Config struct {
		RepoURL          string `json:"repo_url,omitempty"`
		ChartPath        string `json:"chart_path,omitempty"`
		ChartsDir        string `json:"charts_dir,omitempty"`
		SaveDir          string `json:"save_dir,omitempty"`
		PreviousCommitID string `json:"previous_commit_id,omitempty"`
		CurrentCommitID  string `json:"current_commit_id,omitempty"`
	}

	// Plugin struct
	Plugin struct {
		Config     *Config
		Repository *git.Repository
		Commit     *git.Commit
		Client     *cm.Client

		fullChartPath string
	}

	// Chart holds path and parsed helmignore Rules
	Chart struct {
		Path  string
		Rules *ignore.Rules
	}
)

// ValidateConfig validates plugin configuration
func (p *Plugin) ValidateConfig() error {
	var err error
	// validate ChartMuseum baseURL
	if p.Client, err = cm.NewClient(p.Config.RepoURL, nil); err != nil {
		return errors.Wrapf(err, "Could not create ChartMuseum client (repo-url: %q)", p.Config.RepoURL)
	}

	// validate charts-dir is a directory
	if fi, err := os.Stat(p.Config.ChartsDir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("charts-dir: %q is not a directory", p.Config.ChartsDir)
		}
	} else {
		return errors.Wrapf(err, "charts-dir: Could not get file stats for %q", p.Config.ChartsDir)
	}

	if p.Config.CurrentCommitID != "" {
		// validate ChartsDir is a valid repository
		if p.Repository, err = git.OpenRepository(p.Config.ChartsDir); err != nil {
			return errors.Wrapf(err, "Error getting git repository for charts-dir: %q", p.Config.ChartsDir)
		}

		// validate CurrentCommitID is a valid commit in the repository
		if p.Commit, err = p.Repository.GetCommit(p.Config.CurrentCommitID); err != nil {
			return errors.Wrapf(err, "Error getting commit current-commit: %q", p.Config.CurrentCommitID)
		}
	}

	if p.Config.ChartPath != "" {
		p.fullChartPath = filepath.Join(p.Config.ChartsDir, p.Config.ChartPath)
		// validate chart-path is a valid chart
		if valid, err := chartutil.IsChartDir(p.fullChartPath); !valid {
			return errors.Wrapf(err, "Error validating chart-path: %q", p.fullChartPath)
		}
	}

	return nil
}

func (p *Plugin) exec() error {
	ctx := context.Background()
	//ctx, cancel := context.WithTimeout(ctx, 60*time.Second)

	err := p.ValidateConfig()
	if err != nil {
		return err
	}

	var charts []string
	if charts, err = p.discoverCharts(); err != nil {
		return errors.Wrap(err, "Unable to discover charts")
	}

	os.MkdirAll(p.Config.SaveDir, os.ModePerm)
	for _, chart := range charts {
		response, err := p.packageAndUpload(ctx, chart)
		if err != nil {
			fmt.Printf("Ignoring error while processing %q: %+v\n", chart, err)
			continue
		} else if response.Saved {
			fmt.Printf("Succesfully Uploaded %q\n", chart)
		} else {
			fmt.Printf("Unexpected ChartMuseum response (Message = %q)\n", response.Message)
		}
	}

	return nil
}

// packageAndUpload saves a helm chart directory to a compressed package and uploads it to chartmuseum
func (p *Plugin) packageAndUpload(ctx context.Context, chart string) (*cm.Response, error) {
	c, err := chartutil.LoadDir(chart)
	if err != nil {
		return nil, errors.Wrapf(err, "Error while loading Chart directory: %q", chart)
	}

	chartPackage, err := chartutil.Save(c, p.Config.SaveDir)
	if err != nil {
		return nil, errors.Wrapf(err, "Error while packaging Chart: %q", chart)
	}

	f, err := os.Open(chartPackage)
	if err != nil {
		return nil, errors.Wrapf(err, "Error while opening generated Chart package: %q", chartPackage)
	}

	log.Printf("Uploading Chart %v ...\n", chartPackage)
	return p.Client.ChartService.UploadChart(ctx, f)
}

// discoverCharts finds charts based on plugin configuration
func (p *Plugin) discoverCharts() (charts []string, err error) {
	if p.Config.ChartPath != "" {
		charts = []string{p.fullChartPath}
	}

	if p.Config.CurrentCommitID != "" {
		modifiedCharts, err := p.findModifiedCharts()
		if err != nil {
			return nil, errors.Wrapf(err, "Could not find modified Charts")
		}
		if p.Config.ChartPath != "" {
			if _, modified := modifiedCharts[p.fullChartPath]; !modified {
				fmt.Printf("chart: %q wasn't modified.. nothing to do", p.fullChartPath)
				return nil, nil
			}
		} else {
			charts = util.Keys(modifiedCharts)
		}
	} else if p.Config.ChartPath == "" {
		charts, err = p.findAllCharts()
	}
	return charts, err
}

// findAllCharts recursively finds all charts within the configured charts-dir
func (p *Plugin) findAllCharts() (charts []string, err error) {
	fmt.Printf("Finding all charts...\n")
	walk := func(path string, stat os.FileInfo, err error) error {
		if stat != nil && stat.IsDir() {
			fmt.Printf("testing %s\n", path)
			if ok, _ := chartutil.IsChartDir(path); ok {
				fmt.Println("\tFound chart! moving on ...")
				charts = append(charts, path)
				return filepath.SkipDir
			}
		}
		return nil
	}
	err = filepath.Walk(p.Config.ChartsDir, walk)
	return charts, err
}

// findModifiedCharts returns a map of all modified Charts filtered by .helmignore
func (p *Plugin) findModifiedCharts() (map[string]bool, error) {
	fmt.Printf("Getting diff between %v and %v ...\n", p.Config.PreviousCommitID, p.Config.CurrentCommitID)
	lookupCache := make(map[string]*Chart)
	modifiedCharts := make(map[string]bool)
	files, err := p.Commit.GetFilesChangedSinceCommit(p.Config.PreviousCommitID)
	if err != nil {
		return nil, errors.Wrapf(err, "Error while getting files between commit: %q and %q", p.Config.PreviousCommitID, p.Config.CurrentCommitID)
	}
	//fmt.Printf("%#v\n", files)
	for _, file := range files {
		//ignore blank files (seems GetFilesChangedSinceCommit always returns an empty last line)
		if file == "" {
			continue
		}
		fullPath := filepath.Join(p.Config.ChartsDir, file)
		fi, err := os.Stat(fullPath)
		if err != nil {
			fmt.Printf("\tIgnoring modified file %q due to error: %v\n", file, err)
			continue // with next modified file
		}
		dirName := fullPath
		if !fi.IsDir() {
			dirName = filepath.Dir(dirName)
		}
		c, err := getChart(dirName, p.Config.ChartsDir, lookupCache)
		if err != nil {
			fmt.Printf("\tIgnoring modified file %q: %v\n", file, err)
			continue // with next modified file
		}

		fmt.Printf("\tfile %q belongs to %q\n", file, c.Path)

		// flag chart modified if modified file not helmignored
		ignored, err := p.testIgnored(file, c)
		if err != nil {
			fmt.Println(err)
		}
		if !ignored {
			fmt.Printf("\t\tfile %q not ignored!\n", file)
			modifiedCharts[c.Path] = true
		}
	}
	return modifiedCharts, nil
}

func (p *Plugin) testIgnored(file string, c *Chart) (bool, error) {
	path := p.Config.ChartsDir
	// use filepath.Separator ...
	for _, pathSegment := range strings.Split(file, "/") {
		fmt.Printf("\t\t\tpath: %q, pathSegment: %q\n", path, pathSegment)
		path = path + "/" + pathSegment

		fi, err := os.Stat(path)
		if err != nil {
			return false, errors.Wrapf(err, "Error getting %q", path)
		}

		if c.Rules.Ignore(path, fi) {
			fmt.Printf("\t\t\tfile %q is ignored!\n", file)
			return true, nil
		}
	}
	return false, nil
}

// getChart recursively walks up the file tree to find the chart a directory belongs to
// Bug(vincent) this expects chartsDir to be valid prefix of filepath (both relative or absolute?)
func getChart(dirName string, chartsDir string, cache map[string]*Chart) (*Chart, error) {
	//fmt.Printf("\t\ttesting %q ...\n", dirName)
	if cachedChart, ok := cache[dirName]; ok {
		fmt.Printf("\t\tCache hit %q!\n", cachedChart.Path)
		return cachedChart, nil
	}

	c := &Chart{}
	if ok, _ := chartutil.IsChartDir(dirName); ok {
		fmt.Printf("\t\tChart found %q\n", dirName)
		c.Path = dirName
		c.Rules = ignore.Empty()
		err := parseHelmIgnoreRules(c)
		if err != nil {
			return c, errors.Wrapf(err, "Error parsing .helmignore for %s", c.Path)
		}
		c.Rules.AddDefaults()

		cache[dirName] = c
		return c, nil
	}

	// check for root
	if strings.TrimPrefix(dirName, chartsDir) == "" {
		return c, fmt.Errorf("No chart in parent directory chain")
	}

	// recursive find chart in parent directory chain
	c, err := getChart(filepath.Dir(dirName), chartsDir, cache)
	if err != nil {
		return c, errors.Wrapf(err, "Error getting parent chart for %s", dirName)
	}
	cache[dirName] = c
	return c, nil
}

// parseHelmIgnoreRules detects and loads custom helmignore Rules
func parseHelmIgnoreRules(c *Chart) error {
	ifile := filepath.Join(c.Path, ignore.HelmIgnore)
	if _, err := os.Stat(ifile); err == nil {
		r, err := ignore.ParseFile(ifile)
		if err != nil {
			return err
		}
		c.Rules = r
	}
	return nil
}
