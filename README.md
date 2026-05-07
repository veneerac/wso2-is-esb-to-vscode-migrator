# migrate-is-to-vscode

A Go script that migrates **WSO2 Integration Studio** multi-module Maven projects
to the flat **VS Code Extension (MI 4.x)** project structure — no VS Code extension required.

---

## Prerequisites

- **Go 1.18+** installed → verify with:
  ```bash
  go version
  ```

---

## Folder Structure

```
migrate-is-to-vscode/
├── main.go          ← migration script
├── go.mod
├── input/           ← drop old Integration Studio projects here
├── output/          ← migrated VS Code projects appear here
└── logs/
    └── <project>/   ← one folder per project
        └── migration-<project>.log
```

---

## Steps to Run

### Step 1 — Build (once — also creates all required folders)

```bash
make build
```

This does three things in one command:
- Compiles `main.go` → creates the `migrate` binary
- Creates `input/`, `output/`, `logs/` folders ready to use

### Step 2 — Drop your old project(s) into `input/`

```bash
cp -r /path/to/my-old-studio-project  ./input/
```

> You can drop multiple projects at once — all will be migrated in one run.

### Step 3 — Run the migration

```bash
./migrate
```

### That's it. From Step 2 onwards every migration is just:

```bash
cp -r /path/to/another-project ./input/
./migrate
```

### Step 5 — Find your migrated project in `output/`

```
output/
└── my-project-car/
    ├── pom.xml
    ├── mvnw
    ├── .env
    ├── .vscode/settings.json
    ├── deployment/
    │   ├── deployment.toml
    │   └── docker/Dockerfile
    └── src/main/
        ├── java/                            ← Java mediators (merged)
        └── wso2mi/
            ├── artifacts/
            │   ├── apis/
            │   ├── sequences/
            │   └── templates/
            └── resources/
                └── registry/               ← Registry resources (merged)
```

### Step 6 — Check the log

```
logs/
└── my-old-studio-project/
    └── migration-my-old-studio-project.log
```

---

## Optional Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-input-dir` | `./input` | Folder containing old IS projects |
| `-output-dir` | `./output` | Folder where migrated projects are written |
| `-logs-dir` | `./logs` | Folder where log subfolders are created |
| `-dry-run` | `false` | Print planned actions without writing any files |

### Examples

```bash
# Preview only — nothing is written
./migrate -dry-run

# Custom folder locations
./migrate -input-dir /old/projects -output-dir /migrated -logs-dir /migration-logs

# Dry-run with custom paths
./migrate -input-dir /old/projects -output-dir /migrated -dry-run
```

---

## What the Script Migrates

| Old (Integration Studio) | New (VS Code Extension) |
|--------------------------|-------------------------|
| `*-config/src/main/synapse-config/api/` | `src/main/wso2mi/artifacts/apis/` |
| `*-config/src/main/synapse-config/sequences/` | `src/main/wso2mi/artifacts/sequences/` |
| `*-config/src/main/synapse-config/templates/` | `src/main/wso2mi/artifacts/templates/` |
| `*-registry/*.json / *.xslt` | `src/main/wso2mi/resources/registry/conf/.../` |
| `*-mediators/src/main/java/` | `src/main/java/` |
| 4 separate Maven modules | 1 single Maven project |
| Eclipse `.project` / `.meta/` files | Removed |
| Old ESB build plugin | `vscode-car-plugin` |
| No Docker support | `deployment/docker/Dockerfile` + `deployment.toml` |

---

## Supported Artifact Types

The scaffold creates directories for all standard WSO2 MI artifact types:

- `apis` · `sequences` · `templates` · `endpoints`
- `proxy-services` · `local-entries` · `inbound-endpoints`
- `message-stores` · `message-processors` · `tasks`

---

## Connector Migration

WSO2 Integration Studio projects may reference connectors via XML tags like `<fileconnector.read>` or `<salesforcerest.query>`. MI 4.x uses a different connector syntax, so the script cannot auto-migrate these safely. Instead it:

1. **Scans every XML file** in the project for connector-style tags (`<name.operation>`).
2. **Logs a WARNING** for each connector found — visible in both the console output and the per-project log file.
3. **Creates a `connectors/` folder** inside the migrated project with one init-template file per detected connector.
4. **Writes `connectors/REVIEW_REQUIRED.md`** — a checklist explaining what to review before building.

### Known connector mappings

| Old IS connector tag prefix | Action taken | MI 4.x init file generated |
|-----------------------------|--------------|----------------------------|
| `fileconnector` / `file` | Template auto-generated | `connectors/file.init` (uses new `file.init` local-entry syntax) |
| `salesforcerest` | Existing init copied (if found), else placeholder written | `connectors/salesforcerest.init` |
| Any other connector | Placeholder file written | `connectors/<name>.init` |

> The `file.init` template is generated with the MI 4.x `<file.init>` element. You still need to verify the `workingDir` path matches your environment.

### After migration — connector review steps

1. Open `connectors/REVIEW_REQUIRED.md` inside the migrated project.
2. For each file in `connectors/`:
   - Verify the connection parameters (host, credentials, paths).
   - Copy the reviewed file into `src/main/wso2mi/artifacts/local-entries/`.
3. Delete the `connectors/` folder once all files have been moved.
4. Build the project — connector warnings will disappear once the local-entry files are in place.

> **Note:** Standard Synapse mediators (`call`, `log`, `filter`, `payloadFactory`, etc.) are excluded from connector detection and will never generate a warning.

---

## Troubleshooting

**No projects found in input/**
> Make sure the folder you copied is the root of the IS project (the one containing `pom.xml` with `<modules>`).

**Module not detected (shows "not found")**
> The script auto-detects module roles. If a module is non-standard, check that:
> - Config module has `src/main/synapse-config/`
> - Registry module has `artifact.xml` containing `registry/resource`
> - Mediators module has `src/main/java/`

**Registry path mapping**
> Registry paths are mapped as:
> - `/_system/config/...` → `conf/...`
> - `/_system/governance/...` → `governance/...`
