package embeddedpostgres

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	_ "github.com/lib/pq"
	"github.com/mholt/archiver"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	version      PostgresVersion
	port         uint32
	database     string
	username     string
	password     string
	runtimePath  string
	startTimeout time.Duration
	stopTimeout  time.Duration
}

func DefaultConfig() Config {
	return Config{
		version:      V12_1_0,
		port:         5432,
		database:     "postgres",
		username:     "postgres",
		password:     "postgres",
		startTimeout: 15 * time.Second,
		stopTimeout:  5 * time.Second,
	}
}

func (c Config) Version(version PostgresVersion) Config {
	c.version = version
	return c
}

func (c Config) Port(port uint32) Config {
	c.port = port
	return c
}

func (c Config) Database(database string) Config {
	c.database = database
	return c
}

func (c Config) Username(username string) Config {
	c.username = username
	return c
}

func (c Config) Password(password string) Config {
	c.password = password
	return c
}

func (c Config) RuntimePath(path string) Config {
	c.runtimePath = path
	return c
}

func (c Config) StartTimeout(timeout time.Duration) Config {
	c.startTimeout = timeout
	return c
}

func (c Config) StopTimeout(timeout time.Duration) Config {
	c.stopTimeout = timeout
	return c
}

type PostgresVersion string

const (
	V12_1_0  = "12.1.0"
	V12_0_0  = "12.0.0"
	V11_6_0  = "11.6.0"
	V11_5_0  = "11.5.0"
	V11_4_0  = "11.4.0"
	V11_3_0  = "11.3.0"
	V11_2_0  = "11.2.0"
	V11_1_0  = "11.1.0"
	V11_0_0  = "11.0.0"
	V10_11_0 = "10.11.0"
	V10_10_0 = "10.10.0"
	V10_9_0  = "10.9.0"
	V10_8_0  = "10.8.0"
	V10_7_0  = "10.7.0"
	V10_6_0  = "10.6.0"
	V10_5_0  = "10.5.0"
	V10_4_0  = "10.4.0"
	V9_6_16  = "9.6.16"
	V9_6_15  = "9.6.15"
	V9_6_14  = "9.6.14"
	V9_6_13  = "9.6.13"
	V9_6_12  = "9.6.12"
	V9_6_11  = "9.6.11"
	V9_6_10  = "9.6.10"
	V9_6_9   = "9.6.9"
	V9_5_20  = "9.5.20"
	V9_5_19  = "9.5.19"
	V9_5_18  = "9.5.18"
	V9_5_17  = "9.5.17"
	V9_5_16  = "9.5.16"
	V9_5_15  = "9.5.15"
	V9_5_14  = "9.5.14"
	V9_5_13  = "9.5.13"
	V9_4_25  = "9.4.25"
	V9_4_24  = "9.4.24"
	V9_4_23  = "9.4.23"
	V9_4_22  = "9.4.22"
	V9_4_21  = "9.4.21"
	V9_4_20  = "9.4.20"
	V9_4_19  = "9.4.19"
	V9_4_18  = "9.4.18"
	V9_3_25  = "9.3.25"
	V9_3_24  = "9.3.24"
	V9_3_23  = "9.3.23"
)

type CacheLocator func() (string, bool)

func defaultCacheLocator(versionStrategy VersionStrategy) CacheLocator {
	return func() (string, bool) {
		var cacheDirectory string
		if userHome, err := os.UserHomeDir(); err != nil {
			cacheDirectory = ".embedded-postgres-go"
		} else {
			cacheDirectory = filepath.Join(userHome, ".embedded-postgres-go")
		}
		operatingSystem, architecture, version := versionStrategy()
		cacheLocation := filepath.Join(cacheDirectory,
			fmt.Sprintf("embedded-postgres-binaries-%s-%s-%s.txz",
				operatingSystem,
				architecture,
				version))
		info, err := os.Stat(cacheLocation)
		if err != nil {
			return cacheLocation, os.IsExist(err) && !info.IsDir()
		}
		return cacheLocation, !info.IsDir()
	}
}

type VersionStrategy func() (string, string, PostgresVersion)

func defaultVersionStrategy(config Config) VersionStrategy {
	return func() (operatingSystem, architecture string, version PostgresVersion) {
		return runtime.GOOS, runtime.GOARCH, config.version
	}
}

type EmbeddedPostgres struct {
	config              Config
	cacheLocator        CacheLocator
	remoteFetchStrategy RemoteFetchStrategy
	shutdownHook        chan bool
	startupHook         chan bool
}

func NewDatabase() *EmbeddedPostgres {
	return newDatabaseWithConfig(DefaultConfig())
}

func NewDatabaseWithConfig(config Config) *EmbeddedPostgres {
	return newDatabaseWithConfig(config)
}

func newDatabaseWithConfig(config Config) *EmbeddedPostgres {
	versionStrategy := defaultVersionStrategy(config)
	cacheLocator := defaultCacheLocator(versionStrategy)
	remoteFetchStrategy := defaultRemoteFetchStrategy(versionStrategy, cacheLocator)
	return &EmbeddedPostgres{
		config:              config,
		cacheLocator:        cacheLocator,
		remoteFetchStrategy: remoteFetchStrategy,
		shutdownHook:        make(chan bool, 1),
	}
}

func (ep *EmbeddedPostgres) Start() error {
	conn, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", ep.config.port))
	if err != nil {
		return fmt.Errorf("process already listening on port %d", ep.config.port)
	}
	if err := conn.Close(); err != nil {
		return err
	}

	cacheLocation, exists := ep.cacheLocator()
	if !exists {
		if err := ep.remoteFetchStrategy(); err != nil {
			return err
		}
	}
	binaryExtractLocation := userLocationOrDefault(ep.config.runtimePath, cacheLocation)
	if err := os.RemoveAll(binaryExtractLocation); err != nil {
		return fmt.Errorf("unable to clean up directory %s with error: %s", binaryExtractLocation, err)
	}
	if err := archiver.NewTarXz().Unarchive(cacheLocation, binaryExtractLocation); err != nil {
		return fmt.Errorf("unable to extract postgres archive %s to %s with error: %s", cacheLocation, binaryExtractLocation, err)
	}

	pwfileLocation := filepath.Join(binaryExtractLocation, "pwfile")
	if err := ioutil.WriteFile(pwfileLocation, []byte(ep.config.password), 0600); err != nil {
		return fmt.Errorf("unable to write password file with error: %s", err)
	}
	postgresInitDbBinary := filepath.Join(binaryExtractLocation, "bin/initdb")
	postgresInitDbProcess := exec.Command(postgresInitDbBinary,
		"-A", "password",
		"-U", ep.config.username,
		"-D", filepath.Join(binaryExtractLocation, "data"),
		fmt.Sprintf("--pwfile=%s", pwfileLocation))
	postgresInitDbProcess.Stderr = os.Stderr
	postgresInitDbProcess.Stdout = os.Stdout
	if err := postgresInitDbProcess.Run(); err != nil {
		return fmt.Errorf("unable to init database with error: %s", err)
	}
	ep.startupHook = make(chan bool, 1)
	go ep.startPostgres(binaryExtractLocation)
	for range ep.startupHook {
	}
	return nil
}

func (ep *EmbeddedPostgres) startPostgres(binaryExtractLocation string) {
	postgresBinary := filepath.Join(binaryExtractLocation, "bin/postgres")
	postgresProcess := exec.Command(postgresBinary, "-p", fmt.Sprintf("%d", ep.config.port), "-h", "localhost", "-D", filepath.Join(binaryExtractLocation, "data"))
	postgresProcess.Stderr = os.Stderr
	postgresProcess.Stdout = os.Stdout
	if err := postgresProcess.Start(); err != nil {
		close(ep.startupHook)
		panic(err)
	}

	complete := make(chan struct{})
	ctx, cancelFunc := context.WithTimeout(context.Background(), ep.config.startTimeout)
	defer cancelFunc()

	go func() {
		for ctx.Err() == nil {
			if err := healthCheckDatabase(ep.config.port, ep.config.username, ep.config.password); err != nil {
				continue
			}
			if err := createDatabase(ep.config.port, ep.config.username, ep.config.password, ep.config.database); err != nil {
				continue
			}
			complete <- struct{}{}
			break
		}
	}()

	select {
	case <-complete:
		close(complete)
		close(ep.startupHook)
	case <-ctx.Done():
		ep.shutdownHook <- true
		close(ep.startupHook)
	}

	for shutdown := range ep.shutdownHook {
		if shutdown {
			if err := postgresProcess.Process.Signal(syscall.SIGQUIT); err != nil {
				log.Println(err)
			}
			if err := postgresProcess.Wait(); err != nil {
				log.Println(err)
			}
			close(ep.shutdownHook)
		}
	}
}

func createDatabase(port uint32, username, password, database string) (funcErr error) {
	if database == "postgres" {
		return nil
	}
	db, err := sql.Open("postgres", fmt.Sprintf("host=localhost port=%d user=%s password=%s dbname=%s sslmode=disable",
		port,
		username,
		password,
		"postgres"))
	defer func() {
		if err := db.Close(); err != nil {
			funcErr = err
		}
	}()
	if err != nil {
		return err
	}
	if _, err := db.Exec(fmt.Sprintf("CREATE DATABASE %s", database)); err != nil {
		return err
	}

	return nil
}

func (ep *EmbeddedPostgres) Stop() error {
	if ep.startupHook == nil {
		return errors.New("postgres not yet started")
	}
	ep.shutdownHook <- true
	for range ep.shutdownHook {
	}
	return nil
}

func healthCheckDatabase(port uint32, username, password string) (funcErr error) {
	db, err := sql.Open("postgres", fmt.Sprintf("host=localhost port=%d user=%s password=%s dbname=%s sslmode=disable",
		port,
		username,
		password,
		"postgres"))
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			funcErr = err
		}
	}()
	rows, err := db.Query("SELECT 1")
	if err != nil {
		return err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			funcErr = err
		}
	}()
	return nil
}

func userLocationOrDefault(userLocation, cacheLocation string) string {
	if userLocation != "" {
		return userLocation
	}
	return filepath.Join(filepath.Dir(cacheLocation), "extracted")
}

type RemoteFetchStrategy func() error

func defaultRemoteFetchStrategy(versionStrategy VersionStrategy, cacheLocator CacheLocator) RemoteFetchStrategy {
	return func() error {
		operatingSystem, architecture, version := versionStrategy()
		downloadUrl := fmt.Sprintf("https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-%s-%s/%s/embedded-postgres-binaries-%s-%s-%s.jar",
			operatingSystem,
			architecture,
			version,
			operatingSystem,
			architecture,
			version)
		resp, err := http.Get(downloadUrl)
		defer func() {
			if err := resp.Body.Close(); err != nil {
				log.Fatal(resp.Body.Close())
			}
		}()
		if err != nil {
			return err
		}
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		zipFile := archiver.NewZip()
		if err := zipFile.Open(bytes.NewReader(bodyBytes), resp.ContentLength); err != nil {
			return err
		}
		defer func() {
			if err := zipFile.Close(); err != nil {
				log.Fatal(err)
			}
		}()
		for {
			downloadedArchive, err := zipFile.Read()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				} else {
					return err
				}
			}
			if header, ok := downloadedArchive.Header.(zip.FileHeader); !ok || !strings.HasSuffix(header.Name, ".txz") {
				continue
			}
			downloadedArchiveBytes, err := ioutil.ReadAll(downloadedArchive)
			if err != nil {
				return err
			}
			cacheLocation, _ := cacheLocator()
			if err := createArchiveFile(cacheLocation, downloadedArchiveBytes); err != nil {
				return err
			}
		}

		return nil
	}
}

func createArchiveFile(archiveLocation string, archiveBytes []byte) error {
	if err := os.MkdirAll(filepath.Dir(archiveLocation), 0755); err != nil {
		return err
	}
	filesystemArchive, err := os.Create(archiveLocation)
	defer func() {
		log.Println(archiveLocation)
		if err := filesystemArchive.Close(); err != nil {
			log.Println(err)
		}
	}()
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(filesystemArchive.Name(), archiveBytes, 0666); err != nil {
		return err
	}
	return nil
}