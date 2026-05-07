// migrate-is-to-vscode
//
// Drop old WSO2 Integration Studio projects into the "input/" folder,
// then run:
//
//   go run main.go
//
// Migrated projects will appear in "output/<project-name>/".
// A detailed migration.log is written inside each output project folder.
//
// Optional flags:
//   -input-dir  <path>   override the input  folder (default: ./input)
//   -output-dir <path>   override the output folder (default: ./output)
//   -dry-run             print planned actions without writing files
package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"
)

// ---------------------------------------------------------------------------
// Tee logger — writes to stdout AND a log file simultaneously
// ---------------------------------------------------------------------------

type Logger struct {
	w       io.Writer // both stdout + file
	file    *os.File
	dryRun  bool
	start   time.Time // overall start
	stepStart time.Time
}

func newLogger(logPath string, dryRun bool) (*Logger, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	return &Logger{
		w:      io.MultiWriter(os.Stdout, f),
		file:   f,
		dryRun: dryRun,
		start:  time.Now(),
	}, nil
}

func (l *Logger) Close() { l.file.Close() }

func (l *Logger) log(format string, a ...any) {
	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(l.w, "[%s] "+format+"\n", append([]any{ts}, a...)...)
}

func (l *Logger) section(title string) {
	l.stepStart = time.Now()
	fmt.Fprintf(l.w, "\n[%s] ─── %s\n", time.Now().Format("15:04:05"), title)
}

func (l *Logger) stepDone() {
	fmt.Fprintf(l.w, "[%s]     ✓ done  (+%s)\n",
		time.Now().Format("15:04:05"),
		time.Since(l.stepStart).Round(time.Millisecond))
}

func (l *Logger) action(verb, detail string) {
	prefix := "     "
	if l.dryRun {
		prefix = "     [DRY] "
	}
	fmt.Fprintf(l.w, "[%s]%s%-6s %s\n",
		time.Now().Format("15:04:05"), prefix, verb, detail)
}

func (l *Logger) elapsed() string {
	return time.Since(l.start).Round(time.Millisecond).String()
}

func (l *Logger) warn(format string, a ...any) {
	ts := time.Now().Format("15:04:05")
	fmt.Fprintf(l.w, "[%s] ⚠  WARNING: "+format+"\n", append([]any{ts}, a...)...)
}

// ---------------------------------------------------------------------------
// XML types
// ---------------------------------------------------------------------------

type PomProject struct {
	XMLName    xml.Name  `xml:"project"`
	GroupID    string    `xml:"groupId"`
	ArtifactID string    `xml:"artifactId"`
	Version    string    `xml:"version"`
	Modules    []string  `xml:"modules>module"`
	Parent     PomParent `xml:"parent"`
}

type PomParent struct {
	GroupID string `xml:"groupId"`
	Version string `xml:"version"`
}

type RegistryArtifacts struct {
	XMLName   xml.Name           `xml:"artifacts"`
	Artifacts []RegistryArtifact `xml:"artifact"`
}

type RegistryArtifact struct {
	Name       string         `xml:"name,attr"`
	GroupID    string         `xml:"groupId,attr"`
	Version    string         `xml:"version,attr"`
	Type       string         `xml:"type,attr"`
	ServerRole string         `xml:"serverRole,attr"`
	Items      []RegistryItem `xml:"item"`
}

type RegistryItem struct {
	File      string `xml:"file"`
	Path      string `xml:"path"`
	MediaType string `xml:"mediaType"`
}

// ---------------------------------------------------------------------------
// Project metadata
// ---------------------------------------------------------------------------

type ProjectInfo struct {
	GroupID    string
	ArtifactID string
	Version    string

	ConfigModule    string
	RegistryModule  string
	MediatorsModule string
	CarModule       string
}

// ---------------------------------------------------------------------------
// Folder & path helpers
// ---------------------------------------------------------------------------

var synapsefolderMap = map[string]string{"api": "apis"}

func mapSynapseFolder(s string) string {
	if v, ok := synapsefolderMap[s]; ok {
		return v
	}
	return s
}

// /_system/config/foo/bar  →  conf/foo/bar
// /_system/governance/foo  →  governance/foo
func registryPathToFolder(p string) string {
	p = strings.TrimPrefix(p, "/")
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 2 {
		return p
	}
	switch parts[1] {
	case "config":
		if len(parts) >= 3 {
			return "conf/" + parts[2]
		}
		return "conf"
	case "governance":
		if len(parts) >= 3 {
			return "governance/" + parts[2]
		}
		return "governance"
	default:
		return strings.Join(parts[1:], "/")
	}
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	project   := flag.String("project",    "",       "Path to a single old IS project to migrate (optional)")
	inputDir  := flag.String("input-dir",  "input",  "Folder containing old Integration Studio projects")
	outputDir := flag.String("output-dir", "output", "Folder where migrated projects will be written")
	logsDir   := flag.String("logs-dir",   "logs",   "Folder where per-project log folders are created")
	dryRun    := flag.Bool("dry-run", false, "Print actions without writing files")
	flag.Parse()

	// Ensure output/logs dirs always exist
	for _, d := range []string{*outputDir, *logsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "cannot create directory %s: %v\n", d, err)
			os.Exit(1)
		}
	}

	// Collect projects to migrate
	var projects []string

	if *project != "" {
		// Single project passed directly via -project flag — no input/ folder needed
		if !strings.Contains(string(mustRead(filepath.Join(*project, "pom.xml"))), "<modules>") {
			fmt.Fprintf(os.Stderr, "ERROR: %q does not look like a multi-module Integration Studio project\n", *project)
			os.Exit(1)
		}
		projects = []string{*project}
	} else {
		// Folder-based mode: scan input/ for projects
		if err := os.MkdirAll(*inputDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "cannot create directory %s: %v\n", *inputDir, err)
			os.Exit(1)
		}
		var err error
		projects, err = findProjects(*inputDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scanning input dir: %v\n", err)
			os.Exit(1)
		}
		if len(projects) == 0 {
			fmt.Printf("No projects found in %q.\n\n", *inputDir)
			fmt.Println("You have two options:")
			fmt.Println("  1. Drop projects into the input/ folder and run:  ./migrate")
			fmt.Println("  2. Point directly at a project and run:           ./migrate -project /path/to/old-project")
			os.Exit(0)
		}
	}

	fmt.Printf("Found %d project(s) to migrate:\n", len(projects))
	for _, p := range projects {
		fmt.Printf("  • %s\n", p)
	}
	fmt.Println()

	overallStart := time.Now()
	success, failed := 0, 0

	for _, projectPath := range projects {
		projectName := filepath.Base(projectPath)

		// logs/<project-name>/migration-<project-name>.log
		logFolder := filepath.Join(*logsDir, projectName)
		if err := os.MkdirAll(logFolder, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "cannot create %s: %v\n", logFolder, err)
			failed++
			continue
		}

		logPath := filepath.Join(logFolder, "migration-"+projectName+".log")
		logger, err := newLogger(logPath, *dryRun)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open log file: %v\n", err)
			failed++
			continue
		}

		logger.log("════════════════════════════════════════════════════")
		logger.log("  WSO2 IS → VS Code Migration")
		logger.log("  Project : %s", projectName)
		logger.log("  Input   : %s", projectPath)
		logger.log("  Output  : %s", *outputDir)
		logger.log("  Log     : %s", logPath)
		if *dryRun {
			logger.log("  Mode    : DRY-RUN (no files written)")
		}
		logger.log("════════════════════════════════════════════════════")

		err = migrateProject(projectPath, *outputDir, logger, *dryRun)
		logger.log("")
		if err != nil {
			logger.log("FAILED: %v", err)
			logger.log("Total elapsed: %s", logger.elapsed())
			logger.Close()
			failed++
		} else {
			logger.log("Migration successful — total elapsed: %s", logger.elapsed())
			logger.log("Log written to: logs/%s/migration-%s.log", projectName, projectName)
			logger.Close()
			success++
		}
	}

	fmt.Printf("\n════════════════════════════════════════\n")
	fmt.Printf("  Summary: %d succeeded, %d failed  (total: %s)\n",
		success, failed, time.Since(overallStart).Round(time.Millisecond))
	fmt.Printf("════════════════════════════════════════\n")

	if failed > 0 {
		os.Exit(1)
	}
}

// findProjects scans dir for subdirectories that contain a multi-module pom.xml
func findProjects(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pomPath := filepath.Join(dir, e.Name(), "pom.xml")
		data, err := os.ReadFile(pomPath)
		if err != nil {
			continue
		}
		// Only pick up multi-module projects (Integration Studio root POMs)
		if strings.Contains(string(data), "<modules>") {
			found = append(found, filepath.Join(dir, e.Name()))
		}
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Core migration
// ---------------------------------------------------------------------------

func migrateProject(inputDir, outputDir string, log *Logger, dryRun bool) error {
	// ── Step 1: Parse root pom ────────────────────────────────────────────
	log.section("Step 1/9  Parse root pom.xml")
	info, err := parseRootPom(inputDir)
	if err != nil {
		return fmt.Errorf("parseRootPom: %w", err)
	}
	log.log("  groupId    : %s", info.GroupID)
	log.log("  artifactId : %s", info.ArtifactID)
	log.log("  version    : %s", info.Version)
	log.log("  config     : %s", orNA(info.ConfigModule))
	log.log("  registry   : %s", orNA(info.RegistryModule))
	log.log("  mediators  : %s", orNA(info.MediatorsModule))
	log.log("  CAR module : %s", orNA(info.CarModule))
	log.stepDone()

	// ── Step 2: Determine output path ─────────────────────────────────────
	log.section("Step 2/9  Determine output path")
	carName := info.CarModule
	if carName == "" {
		carName = info.ArtifactID + "-car"
	}
	outCar := filepath.Join(outputDir, carName)
	log.log("  output project : %s", outCar)
	log.stepDone()

	// ── Step 3: Create directory scaffold ─────────────────────────────────
	log.section("Step 3/9  Create directory scaffold")
	dirs := []string{
		"src/main/wso2mi/artifacts/apis",
		"src/main/wso2mi/artifacts/sequences",
		"src/main/wso2mi/artifacts/templates",
		"src/main/wso2mi/artifacts/endpoints",
		"src/main/wso2mi/artifacts/proxy-services",
		"src/main/wso2mi/artifacts/local-entries",
		"src/main/wso2mi/artifacts/inbound-endpoints",
		"src/main/wso2mi/artifacts/message-stores",
		"src/main/wso2mi/artifacts/message-processors",
		"src/main/wso2mi/artifacts/tasks",
		"src/main/wso2mi/resources/conf",
		"src/main/wso2mi/resources/registry",
		"src/main/java",
		"src/test/wso2mi",
		"connectors",
		"deployment/docker/resources",
		".mvn/wrapper",
		".vscode",
	}
	for _, d := range dirs {
		full := filepath.Join(outCar, d)
		log.action("mkdir", d)
		if !dryRun {
			if err := os.MkdirAll(full, 0755); err != nil {
				return err
			}
		}
	}
	log.stepDone()

	// ── Step 4: Synapse config artifacts ──────────────────────────────────
	log.section("Step 4/9  Migrate Synapse config artifacts")
	if info.ConfigModule != "" {
		if err := migrateConfigs(inputDir, info.ConfigModule, outCar, log, dryRun); err != nil {
			return fmt.Errorf("migrateConfigs: %w", err)
		}
	} else {
		log.log("  (no config module found — skipped)")
	}
	log.stepDone()

	// ── Step 5: Registry resources ────────────────────────────────────────
	log.section("Step 5/9  Migrate registry resources")
	if info.RegistryModule != "" {
		if err := migrateRegistry(inputDir, info.RegistryModule, outCar, log, dryRun); err != nil {
			return fmt.Errorf("migrateRegistry: %w", err)
		}
	} else {
		log.log("  (no registry module found — skipped)")
	}
	log.stepDone()

	// ── Step 6: Java mediators ────────────────────────────────────────────
	log.section("Step 6/9  Migrate Java mediators")
	if info.MediatorsModule != "" {
		if err := migrateMediators(inputDir, info.MediatorsModule, outCar, log, dryRun); err != nil {
			return fmt.Errorf("migrateMediators: %w", err)
		}
	} else {
		log.log("  (no mediators module found — skipped)")
	}
	log.stepDone()

	// ── Step 7: pom.xml ───────────────────────────────────────────────────
	log.section("Step 7/9  Generate pom.xml")
	if err := generatePom(info, outCar, log, dryRun); err != nil {
		return fmt.Errorf("generatePom: %w", err)
	}
	log.stepDone()

	// ── Step 8: Deployment files ──────────────────────────────────────────
	log.section("Step 8/9  Generate deployment files")
	if err := generateDeploymentFiles(outCar, log, dryRun); err != nil {
		return fmt.Errorf("generateDeploymentFiles: %w", err)
	}
	log.stepDone()

	// ── Step 9: VS Code settings + Maven wrapper ──────────────────────────
	log.section("Step 9/10  Generate VS Code settings & Maven wrapper")
	if err := generateVSCodeSettings(outCar, log, dryRun); err != nil {
		return fmt.Errorf("generateVSCodeSettings: %w", err)
	}
	if err := generateMavenWrapper(outCar, log, dryRun); err != nil {
		return fmt.Errorf("generateMavenWrapper: %w", err)
	}
	log.stepDone()

	// ── Step 10: Connector detection ──────────────────────────────────────
	log.section("Step 10/10  Detect & migrate connectors")
	if err := migrateConnectors(inputDir, info, outCar, log, dryRun); err != nil {
		return fmt.Errorf("migrateConnectors: %w", err)
	}
	log.stepDone()

	return nil
}

// ---------------------------------------------------------------------------
// Step 1 helpers
// ---------------------------------------------------------------------------

func parseRootPom(inputDir string) (*ProjectInfo, error) {
	data, err := os.ReadFile(filepath.Join(inputDir, "pom.xml"))
	if err != nil {
		return nil, err
	}
	var pom PomProject
	if err := xml.Unmarshal(data, &pom); err != nil {
		return nil, err
	}
	info := &ProjectInfo{
		GroupID:    pom.GroupID,
		ArtifactID: pom.ArtifactID,
		Version:    pom.Version,
	}
	if info.GroupID == "" {
		info.GroupID = pom.Parent.GroupID
	}
	if info.Version == "" {
		info.Version = pom.Parent.Version
	}
	for _, mod := range pom.Modules {
		modDir := filepath.Join(inputDir, mod)
		switch detectModuleRole(modDir) {
		case "config":
			info.ConfigModule = mod
		case "registry":
			info.RegistryModule = mod
		case "mediators":
			info.MediatorsModule = mod
		case "car":
			info.CarModule = mod
		}
	}
	return info, nil
}

func detectModuleRole(modDir string) string {
	if dirExists(filepath.Join(modDir, "src", "main", "synapse-config")) {
		return "config"
	}
	artXML := filepath.Join(modDir, "artifact.xml")
	if fileExists(artXML) {
		if data, err := os.ReadFile(artXML); err == nil {
			if strings.Contains(string(data), "registry/resource") {
				return "registry"
			}
		}
	}
	if dirExists(filepath.Join(modDir, "src", "main", "java")) {
		return "mediators"
	}
	if fileExists(filepath.Join(modDir, "deployment.properties")) || strings.HasSuffix(modDir, "-car") {
		return "car"
	}
	return "unknown"
}

// ---------------------------------------------------------------------------
// Step 4 — Synapse config
// ---------------------------------------------------------------------------

func migrateConfigs(inputDir, configModule, outCar string, log *Logger, dryRun bool) error {
	srcBase := filepath.Join(inputDir, configModule, "src", "main", "synapse-config")
	dstBase := filepath.Join(outCar, "src", "main", "wso2mi", "artifacts")

	entries, err := os.ReadDir(srcBase)
	if err != nil {
		return err
	}
	copied := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		srcFolder := filepath.Join(srcBase, e.Name())
		dstFolderName := mapSynapseFolder(e.Name())
		dstFolder := filepath.Join(dstBase, dstFolderName)

		files, err := os.ReadDir(srcFolder)
		if err != nil {
			return err
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			src := filepath.Join(srcFolder, f.Name())
			dst := filepath.Join(dstFolder, f.Name())
			log.action("copy", fmt.Sprintf("%s/%s  →  artifacts/%s/%s",
				e.Name(), f.Name(), dstFolderName, f.Name()))
			if !dryRun {
				if err := os.MkdirAll(dstFolder, 0755); err != nil {
					return err
				}
				if err := copyFile(src, dst); err != nil {
					return err
				}
			}
			copied++
		}
	}
	log.log("  %d artifact file(s) copied", copied)
	return nil
}

// ---------------------------------------------------------------------------
// Step 5 — Registry resources
// ---------------------------------------------------------------------------

func migrateRegistry(inputDir, registryModule, outCar string, log *Logger, dryRun bool) error {
	modDir := filepath.Join(inputDir, registryModule)
	data, err := os.ReadFile(filepath.Join(modDir, "artifact.xml"))
	if err != nil {
		return fmt.Errorf("reading artifact.xml: %w", err)
	}
	var reg RegistryArtifacts
	if err := xml.Unmarshal(data, &reg); err != nil {
		return fmt.Errorf("parsing artifact.xml: %w", err)
	}

	registryBase := filepath.Join(outCar, "src", "main", "wso2mi", "resources", "registry")
	copied := 0
	for _, art := range reg.Artifacts {
		for _, item := range art.Items {
			folder := registryPathToFolder(item.Path)
			src := filepath.Join(modDir, item.File)
			dstDir := filepath.Join(registryBase, folder)
			dst := filepath.Join(dstDir, item.File)
			log.action("copy", fmt.Sprintf("%s  →  registry/%s/%s",
				item.File, folder, item.File))
			if !dryRun {
				if err := os.MkdirAll(dstDir, 0755); err != nil {
					return err
				}
				if err := copyFile(src, dst); err != nil {
					return err
				}
			}
			copied++
		}
	}
	newArtFile := filepath.Join(registryBase, "artifact.xml")
	log.action("write", "registry/artifact.xml")
	if !dryRun {
		if err := os.WriteFile(newArtFile, data, 0644); err != nil {
			return err
		}
	}
	log.log("  %d registry resource(s) copied", copied)
	return nil
}

// ---------------------------------------------------------------------------
// Step 6 — Java mediators
// ---------------------------------------------------------------------------

func migrateMediators(inputDir, mediatorsModule, outCar string, log *Logger, dryRun bool) error {
	srcJava := filepath.Join(inputDir, mediatorsModule, "src", "main", "java")
	dstJava := filepath.Join(outCar, "src", "main", "java")
	copied := 0

	err := filepath.Walk(srcJava, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(srcJava, path)
		dst := filepath.Join(dstJava, rel)
		if info.IsDir() {
			if !dryRun {
				return os.MkdirAll(dst, 0755)
			}
			return nil
		}
		log.action("copy", fmt.Sprintf("java/%s", rel))
		if !dryRun {
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			if err := copyFile(path, dst); err != nil {
				return err
			}
		}
		copied++
		return nil
	})
	if err != nil {
		return err
	}
	log.log("  %d Java source file(s) copied", copied)
	return nil
}

// ---------------------------------------------------------------------------
// Step 7 — pom.xml
// ---------------------------------------------------------------------------

const pomTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<project xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 https://maven.apache.org/xsd/maven-4.0.0.xsd"
         xmlns="http://maven.apache.org/POM/4.0.0"
         xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
    <modelVersion>4.0.0</modelVersion>
    <groupId>{{.GroupID}}</groupId>
    <artifactId>{{.CarArtifactID}}</artifactId>
    <version>{{.Version}}</version>
    <packaging>jar</packaging>
    <name>{{.CarArtifactID}}</name>
    <description>{{.CarArtifactID}}</description>

    <repositories>
        <repository>
            <id>wso2-nexus</id>
            <name>WSO2 internal Repository</name>
            <url>https://maven.wso2.org/nexus/content/groups/wso2-public/</url>
            <releases><enabled>true</enabled><updatePolicy>daily</updatePolicy><checksumPolicy>ignore</checksumPolicy></releases>
        </repository>
        <repository>
            <id>wso2.releases</id>
            <url>https://maven.wso2.org/nexus/content/repositories/releases/</url>
            <releases><enabled>true</enabled><updatePolicy>daily</updatePolicy><checksumPolicy>ignore</checksumPolicy></releases>
        </repository>
        <repository>
            <id>wso2.snapshots</id>
            <url>https://maven.wso2.org/nexus/content/repositories/snapshots/</url>
            <snapshots><enabled>true</enabled><updatePolicy>daily</updatePolicy></snapshots>
            <releases><enabled>false</enabled></releases>
        </repository>
    </repositories>

    <pluginRepositories>
        <pluginRepository>
            <id>wso2-nexus</id>
            <url>https://maven.wso2.org/nexus/content/groups/wso2-public/</url>
            <releases><enabled>true</enabled><updatePolicy>daily</updatePolicy><checksumPolicy>ignore</checksumPolicy></releases>
        </pluginRepository>
        <pluginRepository>
            <id>wso2.snapshots</id>
            <url>https://maven.wso2.org/nexus/content/repositories/snapshots/</url>
            <snapshots><enabled>true</enabled><updatePolicy>daily</updatePolicy></snapshots>
            <releases><enabled>false</enabled></releases>
        </pluginRepository>
    </pluginRepositories>

    <properties>
        <projectType>integration-project</projectType>
        <project.runtime.version>4.4.0</project.runtime.version>
        <car.plugin.version>5.4.13</car.plugin.version>
        <maven.compiler.source>1.8</maven.compiler.source>
        <maven.compiler.target>1.8</maven.compiler.target>
        <fat.car.enable>false</fat.car.enable>
        <ciphertool.enable>true</ciphertool.enable>
        <keystore.type>JKS</keystore.type>
        <keystore.name>wso2carbon.jks</keystore.name>
        <keystore.password>wso2carbon</keystore.password>
        <keystore.alias>wso2carbon</keystore.alias>
        <dockerfile.base.image>wso2/wso2mi:DOLLAR{project.runtime.version}</dockerfile.base.image>
        <dockerfile.name>{{.CarArtifactID}}:{{.Version}}</dockerfile.name>
        <test.server.type>local</test.server.type>
        <test.server.host>localhost</test.server.host>
        <test.server.port>9008</test.server.port>
        <test.server.path>/</test.server.path>
        <test.server.version>DOLLAR{project.runtime.version}</test.server.version>
        <maven.test.skip>false</maven.test.skip>
    </properties>

    <dependencies>
        <dependency>
            <groupId>org.apache.synapse</groupId>
            <artifactId>synapse-core</artifactId>
            <version>4.0.0-wso2v165</version>
        </dependency>
    </dependencies>

    <profiles>
        <profile>
            <id>default</id>
            <activation><activeByDefault>true</activeByDefault></activation>
            <build>
                <plugins>
                    <plugin>
                        <groupId>org.apache.maven.plugins</groupId>
                        <artifactId>maven-compiler-plugin</artifactId>
                        <configuration><source>1.8</source><target>1.8</target></configuration>
                    </plugin>
                    <plugin>
                        <groupId>org.apache.maven.plugins</groupId>
                        <artifactId>maven-jar-plugin</artifactId>
                        <configuration><skipIfEmpty>true</skipIfEmpty></configuration>
                        <executions>
                            <execution><phase>compile</phase><id>default-jar</id><goals><goal>jar</goal></goals></execution>
                        </executions>
                    </plugin>
                    <plugin>
                        <groupId>org.wso2.maven</groupId>
                        <artifactId>vscode-car-plugin</artifactId>
                        <version>DOLLAR{car.plugin.version}</version>
                        <extensions>true</extensions>
                        <executions>
                            <execution><phase>compile</phase><goals><goal>car</goal></goals></execution>
                        </executions>
                    </plugin>
                    <plugin>
                        <groupId>org.apache.maven.plugins</groupId>
                        <artifactId>maven-dependency-plugin</artifactId>
                        <version>3.5.0</version>
                        <executions>
                            <execution>
                                <phase>process-resources</phase>
                                <goals><goal>copy-dependencies</goal></goals>
                                <configuration>
                                    <outputDirectory>DOLLAR{basedir}/deployment/libs</outputDirectory>
                                    <excludeTransitive>true</excludeTransitive>
                                    <excludeGroupIds>org.apache.synapse,org.apache.axis2</excludeGroupIds>
                                    <excludeTypes>zip,car</excludeTypes>
                                </configuration>
                            </execution>
                        </executions>
                    </plugin>
                    <plugin>
                        <groupId>org.apache.maven.plugins</groupId>
                        <artifactId>maven-install-plugin</artifactId>
                        <version>2.5.2</version>
                        <executions>
                            <execution>
                                <id>install-car</id>
                                <phase>compile</phase>
                                <goals><goal>install-file</goal></goals>
                                <configuration>
                                    <packaging>car</packaging>
                                    <artifactId>DOLLAR{project.artifactId}</artifactId>
                                    <groupId>DOLLAR{project.groupId}</groupId>
                                    <version>DOLLAR{project.version}</version>
                                    <file>DOLLAR{project.build.directory}/DOLLAR{project.artifactId}_DOLLAR{project.version}.car</file>
                                </configuration>
                            </execution>
                        </executions>
                    </plugin>
                </plugins>
            </build>
        </profile>

        <profile>
            <id>docker</id>
            <build>
                <plugins>
                    <plugin>
                        <groupId>org.wso2.maven</groupId>
                        <artifactId>vscode-car-plugin</artifactId>
                        <version>DOLLAR{car.plugin.version}</version>
                        <extensions>true</extensions>
                        <executions>
                            <execution><phase>generate-sources</phase><goals><goal>car</goal></goals></execution>
                        </executions>
                    </plugin>
                    <plugin>
                        <groupId>org.wso2.maven</groupId>
                        <artifactId>mi-container-config-mapper</artifactId>
                        <version>5.2.82</version>
                        <extensions>true</extensions>
                        <executions>
                            <execution>
                                <id>config-mapper-parser</id>
                                <phase>generate-resources</phase>
                                <goals><goal>config-mapper-parser</goal></goals>
                                <configuration>
                                    <miVersion>DOLLAR{project.runtime.version}</miVersion>
                                    <executeCipherTool>DOLLAR{ciphertool.enable}</executeCipherTool>
                                    <keystoreName>DOLLAR{keystore.name}</keystoreName>
                                    <keystoreAlias>DOLLAR{keystore.alias}</keystoreAlias>
                                    <keystoreType>DOLLAR{keystore.type}</keystoreType>
                                    <keystorePassword>DOLLAR{keystore.password}</keystorePassword>
                                    <projectLocation>DOLLAR{project.basedir}</projectLocation>
                                </configuration>
                            </execution>
                        </executions>
                    </plugin>
                    <plugin>
                        <groupId>io.fabric8</groupId>
                        <artifactId>docker-maven-plugin</artifactId>
                        <version>0.45.0</version>
                        <extensions>true</extensions>
                        <executions>
                            <execution>
                                <id>docker-build</id>
                                <phase>package</phase>
                                <goals><goal>build</goal></goals>
                                <configuration>
                                    <images>
                                        <image>
                                            <name>DOLLAR{dockerfile.name}</name>
                                            <build>
                                                <from>DOLLAR{dockerfile.base.image}</from>
                                                <dockerFile>DOLLAR{basedir}/target/tmp_docker/Dockerfile</dockerFile>
                                                <args><BASE_IMAGE>DOLLAR{dockerfile.base.image}</BASE_IMAGE></args>
                                                <useDefaultExcludes>false</useDefaultExcludes>
                                            </build>
                                        </image>
                                    </images>
                                    <verbose>true</verbose>
                                </configuration>
                            </execution>
                        </executions>
                    </plugin>
                </plugins>
            </build>
        </profile>
    </profiles>

    <build>
        <plugins>
            <plugin>
                <groupId>org.wso2.maven</groupId>
                <artifactId>synapse-unit-test-maven-plugin</artifactId>
                <version>5.4.13</version>
                <executions>
                    <execution>
                        <id>synapse-unit-test</id>
                        <phase>test</phase>
                        <goals><goal>synapse-unit-test</goal></goals>
                    </execution>
                </executions>
                <configuration>
                    <server>
                        <testServerType>DOLLAR{test.server.type}</testServerType>
                        <testServerHost>DOLLAR{test.server.host}</testServerHost>
                        <testServerPort>DOLLAR{test.server.port}</testServerPort>
                        <testServerPath>DOLLAR{test.server.path}</testServerPath>
                        <testServerVersion>DOLLAR{test.server.version}</testServerVersion>
                    </server>
                    <mavenTestSkip>DOLLAR{maven.test.skip}</mavenTestSkip>
                </configuration>
            </plugin>
        </plugins>
    </build>
</project>
`

type pomData struct {
	GroupID       string
	CarArtifactID string
	Version       string
}

func generatePom(info *ProjectInfo, outCar string, log *Logger, dryRun bool) error {
	carID := info.CarModule
	if carID == "" {
		carID = info.ArtifactID + "-car"
	}
	src := strings.ReplaceAll(pomTemplate, "DOLLAR", "$")
	tmpl, err := template.New("pom").Parse(src)
	if err != nil {
		return err
	}
	log.action("write", "pom.xml")
	if dryRun {
		return nil
	}
	f, err := os.Create(filepath.Join(outCar, "pom.xml"))
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, pomData{GroupID: info.GroupID, CarArtifactID: carID, Version: info.Version})
}

// ---------------------------------------------------------------------------
// Step 8 — Deployment files
// ---------------------------------------------------------------------------

const deploymentToml = `[server]
hostname = "localhost"

[keystore.primary]
file_name    = "wso2carbon.jks"
password     = "wso2carbon"
alias        = "wso2carbon"
key_password = "wso2carbon"

[truststore]
file_name = "client-truststore.jks"
password  = "wso2carbon"
alias     = "symmetric.key.value"
algorithm = "AES"
`

const dockerfile = `ARG BASE_IMAGE=wso2/wso2mi:4.4.0
FROM ${BASE_IMAGE}

COPY deployment/libs/ /home/wso2carbon/wso2mi-4.4.0/lib/
COPY target/*.car     /home/wso2carbon/wso2mi-4.4.0/repository/deployment/server/carbonapps/
`

const dotEnv = `# Environment variable overrides for WSO2 MI
# Uncomment and edit as needed

# MI_HOST=localhost
# MI_PORT=8290
`

func generateDeploymentFiles(outCar string, log *Logger, dryRun bool) error {
	files := map[string]string{
		filepath.Join("deployment", "deployment.toml"):        deploymentToml,
		filepath.Join("deployment", "docker", "Dockerfile"):   dockerfile,
		".env": dotEnv,
	}
	for rel, content := range files {
		log.action("write", rel)
		if !dryRun {
			dst := filepath.Join(outCar, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, []byte(content), 0644); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Step 9 — VS Code settings + Maven wrapper
// ---------------------------------------------------------------------------

const vscodeSettings = `{
    "MI.runtimes": [
        {
            "name": "MI 4.4.0",
            "path": "/path/to/wso2mi-4.4.0"
        }
    ],
    "MI.activeRuntimeVersion": "4.4.0"
}
`

const mavenWrapperProps = `distributionUrl=https://repo.maven.apache.org/maven2/org/apache/maven/apache-maven/3.9.6/apache-maven-3.9.6-bin.zip
wrapperUrl=https://repo.maven.apache.org/maven2/org/apache/maven/wrapper/maven-wrapper/3.2.0/maven-wrapper-3.2.0.jar
`

const mvnwScript = `#!/bin/sh
set -e

BASEDIR=$(cd "$(dirname "$0")" && pwd)
WRAPPER_JAR="$BASEDIR/.mvn/wrapper/maven-wrapper.jar"
WRAPPER_PROPS="$BASEDIR/.mvn/wrapper/maven-wrapper.properties"

# Download maven-wrapper.jar on first use if not present
if [ ! -f "$WRAPPER_JAR" ]; then
  DOWNLOAD_URL=""
  if [ -f "$WRAPPER_PROPS" ]; then
    DOWNLOAD_URL=$(grep "^wrapperUrl=" "$WRAPPER_PROPS" | cut -d'=' -f2-)
  fi
  if [ -z "$DOWNLOAD_URL" ]; then
    DOWNLOAD_URL="https://repo.maven.apache.org/maven2/org/apache/maven/wrapper/maven-wrapper/3.2.0/maven-wrapper-3.2.0.jar"
  fi
  echo "Downloading Maven Wrapper jar..."
  if command -v curl > /dev/null 2>&1; then
    curl -fsSL -o "$WRAPPER_JAR" "$DOWNLOAD_URL"
  elif command -v wget > /dev/null 2>&1; then
    wget -q -O "$WRAPPER_JAR" "$DOWNLOAD_URL"
  else
    echo "ERROR: curl or wget is required to download maven-wrapper.jar" >&2
    exit 1
  fi
fi

exec java \
  $JAVA_OPTS \
  -classpath "$WRAPPER_JAR" \
  "-Dmaven.multiModuleProjectDirectory=$BASEDIR" \
  org.apache.maven.wrapper.MavenWrapperMain "$@"
`

func generateVSCodeSettings(outCar string, log *Logger, dryRun bool) error {
	dst := filepath.Join(outCar, ".vscode", "settings.json")
	log.action("write", ".vscode/settings.json")
	if !dryRun {
		return os.WriteFile(dst, []byte(vscodeSettings), 0644)
	}
	return nil
}

func generateMavenWrapper(outCar string, log *Logger, dryRun bool) error {
	items := []struct {
		rel  string
		body string
		perm os.FileMode
	}{
		{filepath.Join(".mvn", "wrapper", "maven-wrapper.properties"), mavenWrapperProps, 0644},
		{"mvnw", mvnwScript, 0755},
	}
	for _, item := range items {
		log.action("write", item.rel)
		if !dryRun {
			dst := filepath.Join(outCar, item.rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, []byte(item.body), item.perm); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func orNA(s string) string {
	if s == "" {
		return "(not found)"
	}
	return s
}

func mustRead(path string) []byte {
	data, _ := os.ReadFile(path)
	return data
}

// ---------------------------------------------------------------------------
// Step 10 — Connector detection & migration
// ---------------------------------------------------------------------------

// Standard Synapse/MI built-in element names that look like "word.word"
// but are NOT connectors — exclude these from detection.
var builtinElements = map[string]bool{
	"log.property": true, "call.endpoint": true,
}

// connectorInitTemplates defines auto-generated init templates for connectors
// whose init syntax changed in MI 4.x and need a new local-entry.
var connectorInitTemplates = map[string]string{
	// "fileconnector" is the old IS name → maps to new "file.init" syntax in MI 4.x
	"fileconnector": `<?xml version="1.0" encoding="UTF-8"?>
<!-- File Connector v4.x requires a file.init local-entry (new in MI 4.x).
     Review and update workingDir and other settings before deploying. -->
<localEntry key="{{.Name}}" xmlns="http://ws.apache.org/ns/synapse">
  <file.init>
    <connectionType>LOCAL</connectionType>
    <workingDir>/</workingDir>
    <fileLockScheme>Local</fileLockScheme>
    <fileCacheEnabled>true</fileCacheEnabled>
    <suspendOnConnectionFailure>true</suspendOnConnectionFailure>
    <retriesBeforeSuspension>0</retriesBeforeSuspension>
    <suspendInitialDuration>1000</suspendInitialDuration>
    <suspendProgressionFactor>1.0</suspendProgressionFactor>
    <suspendMaximumDuration>300000</suspendMaximumDuration>
    <name>{{.Name}}</name>
  </file.init>
</localEntry>`,
	"file": `<?xml version="1.0" encoding="UTF-8"?>
<!-- File Connector v4.x requires a file.init local-entry (new in MI 4.x).
     Review and update workingDir and other settings before deploying. -->
<localEntry key="{{.Name}}" xmlns="http://ws.apache.org/ns/synapse">
  <file.init>
    <connectionType>LOCAL</connectionType>
    <workingDir>/</workingDir>
    <fileLockScheme>Local</fileLockScheme>
    <fileCacheEnabled>true</fileCacheEnabled>
    <suspendOnConnectionFailure>true</suspendOnConnectionFailure>
    <retriesBeforeSuspension>0</retriesBeforeSuspension>
    <suspendInitialDuration>1000</suspendInitialDuration>
    <suspendProgressionFactor>1.0</suspendProgressionFactor>
    <suspendMaximumDuration>300000</suspendMaximumDuration>
    <name>{{.Name}}</name>
  </file.init>
</localEntry>`,
}

// connectorTag matches <connectorname.operation ...> patterns in XML
var connectorTagRe = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9_-]+)\.([a-zA-Z][a-zA-Z0-9_-]+)[\s>]`)

func migrateConnectors(inputDir string, info *ProjectInfo, outCar string, log *Logger, dryRun bool) error {
	if info.ConfigModule == "" {
		log.log("  (no config module — skipped)")
		return nil
	}

	// ── 1. Scan all synapse XML files for connector usage ─────────────────
	srcBase := filepath.Join(inputDir, info.ConfigModule, "src", "main", "synapse-config")
	connectors := map[string]bool{} // unique connector names found

	err := filepath.Walk(srcBase, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(path, ".xml") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range connectorTagRe.FindAllSubmatch(data, -1) {
			connName := strings.ToLower(string(match[1]))
			op := strings.ToLower(string(match[2]))
			// skip known built-ins (filter out standard synapse mediators)
			if isSynapseMediatorOrBuiltin(connName) {
				continue
			}
			_ = op
			connectors[connName] = true
		}
		return nil
	})
	if err != nil {
		return err
	}

	if len(connectors) == 0 {
		log.log("  No connectors detected.")
		return nil
	}

	// Sort for deterministic output
	names := make([]string, 0, len(connectors))
	for n := range connectors {
		names = append(names, n)
	}
	sort.Strings(names)

	log.warn("Connectors detected: %s", strings.Join(names, ", "))
	log.warn("Connector init files placed in connectors/ — REVIEW BEFORE BUILDING")

	connectorsDir := filepath.Join(outCar, "connectors")

	// ── 2. Find existing connector init local-entries in the config module ─
	localEntriesDir := filepath.Join(srcBase, "local-entries")
	existingInits := map[string]string{} // connectorName → file path

	if entries, err := os.ReadDir(localEntriesDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".xml") {
				continue
			}
			path := filepath.Join(localEntriesDir, e.Name())
			data, _ := os.ReadFile(path)
			// Check if this local-entry contains a connector init tag
			for connName := range connectors {
				if regexp.MustCompile(`<` + regexp.QuoteMeta(connName) + `\.init[\s>]`).Match(data) {
					existingInits[connName] = path
				}
			}
		}
	}

	// ── 3. For each connector: copy existing init OR generate template ─────
	for _, connName := range names {
		if existingPath, exists := existingInits[connName]; exists {
			// Copy existing init local-entry to connectors/ folder
			dst := filepath.Join(connectorsDir, filepath.Base(existingPath))
			log.action("copy", fmt.Sprintf("connector init: %s  →  connectors/%s",
				filepath.Base(existingPath), filepath.Base(existingPath)))
			log.warn("  [%s] Existing init local-entry copied — verify it is compatible with MI 4.x", connName)
			if !dryRun {
				if err := os.MkdirAll(connectorsDir, 0755); err != nil {
					return err
				}
				if err := copyFile(existingPath, dst); err != nil {
					return err
				}
			}
		} else if tmplStr, known := connectorInitTemplates[connName]; known {
			// Auto-generate a template init local-entry for known connectors
			entryName := fmt.Sprintf("%s-fileConnector-connection-initialization_v1.0", info.ArtifactID)
			fileName := entryName + ".xml"
			dst := filepath.Join(connectorsDir, fileName)
			log.action("generate", fmt.Sprintf("connectors/%s  (template)", fileName))
			log.warn("  [%s] File Connector v4.x requires a NEW file.init local-entry — template generated in connectors/", connName)
			log.warn("  [%s] Update workingDir and settings in connectors/%s before deploying", connName, fileName)
			if !dryRun {
				if err := os.MkdirAll(connectorsDir, 0755); err != nil {
					return err
				}
				tmpl, err := template.New("connInit").Parse(tmplStr)
				if err != nil {
					return err
				}
				f, err := os.Create(dst)
				if err != nil {
					return err
				}
				defer f.Close()
				if err := tmpl.Execute(f, map[string]string{"Name": entryName}); err != nil {
					return err
				}
			}
		} else {
			// Unknown connector — warn developer to create init manually
			log.warn("  [%s] UNKNOWN connector — no init local-entry found and no template available", connName)
			log.warn("  [%s] Manually create a <connectors/%s.init> local-entry in connectors/ before deploying", connName, connName)
		}
	}

	// ── 4. Write REVIEW_REQUIRED.md in connectors/ ────────────────────────
	readme := fmt.Sprintf(`# Connector Review Required

The following connectors were detected in this project and require
manual review before the migrated project can be built and deployed.

Detected connectors: %s

## What to do

1. Review each XML file in this connectors/ folder
2. Update connection settings (hostnames, credentials, paths) as needed
3. Once verified, copy the files to:
   src/main/wso2mi/artifacts/local-entries/

## Why this folder exists

WSO2 MI 4.x requires all connector connections to be defined as
local-entries. Some connectors (e.g. File Connector v4.x) have a
new init syntax that differs from Integration Studio.

Files marked as "(template)" were auto-generated and MUST be
reviewed before use.
`, strings.Join(names, ", "))

	readmePath := filepath.Join(connectorsDir, "REVIEW_REQUIRED.md")
	log.action("write", "connectors/REVIEW_REQUIRED.md")
	if !dryRun {
		if err := os.MkdirAll(connectorsDir, 0755); err != nil {
			return err
		}
		if err := os.WriteFile(readmePath, []byte(readme), 0644); err != nil {
			return err
		}
	}

	log.log("  Connector files written to: connectors/")
	log.log("  → Review connectors/REVIEW_REQUIRED.md before building")
	return nil
}

// isSynapseMediatorOrBuiltin returns true for known Synapse/MI built-in
// element name prefixes that are NOT connectors.
func isSynapseMediatorOrBuiltin(name string) bool {
	builtins := map[string]bool{
		"log": true, "call": true, "respond": true, "property": true,
		"filter": true, "switch": true, "sequence": true, "send": true,
		"drop": true, "enrich": true, "header": true, "fault": true,
		"iterate": true, "aggregate": true, "clone": true, "cache": true,
		"throttle": true, "rewrite": true, "script": true, "smooks": true,
		"payloadFactory": true, "payloadfactory": true, "store": true,
		"builder": true, "entitlement": true, "oauth": true, "bean": true,
		"pojoCommand": true, "pojoccommand": true, "spring": true,
		"publishEvent": true, "publishevent": true, "dataMapper": true,
		"datamapper": true, "dataServiceCall": true, "dataservicecall": true,
		"jsonTransform": true, "jsontransform": true, "rule": true,
		"validate": true, "xslt": true, "fastXSLT": true, "fastxslt": true,
		"event": true, "transaction": true, "variable": true, "foreach": true,
		"scatter": true, "gather": true, "ntlm": true, "inSequence": true,
		"outSequence": true, "faultSequence": true, "insequence": true,
		"outsequence": true, "faultsequence": true, "target": true,
		"address": true, "wsdl": true, "default": true, "description": true,
	}
	return builtins[strings.ToLower(name)]
}
