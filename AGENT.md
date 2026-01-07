# Agent Documentation

This document contains important implementation details, architecture decisions, and troubleshooting information not covered in the README.

## Architecture Overview

### Docker-Based Backup Strategy

The service uses Docker containers to run `pg_dump` and `pg_dumpall` commands. This approach:

- **Version Matching**: Automatically detects PostgreSQL version and uses matching Docker image (e.g., `postgres:17`, `postgres:15`)
- **Compatibility**: Ensures `pg_dump` version matches the database server version, avoiding version mismatch errors
- **Isolation**: Each backup runs in a clean container environment
- **No Host Installation**: No need to install PostgreSQL client tools on the host

### Docker Socket Mounting

The service requires access to the Docker daemon via `/var/run/docker.sock`:

- **Read-only mount**: Socket is mounted read-only for security
- **Container spawning**: Service creates temporary containers for each backup operation
- **Network mode**: Uses `host` network mode so containers can reach external PostgreSQL databases
- **Auto-cleanup**: Containers are automatically removed after backup completes

**Important**: The service must run on a host that has Docker installed and the socket accessible. This works in:
- Docker Compose (socket mounted via volume)
- Kubernetes (socket mounted via hostPath)
- Bare metal servers with Docker installed

## Key Components

### Service Lifecycle

1. **Initialization**:
   - Loads config from environment variables
   - Parses `BACKUP_*` prefixed env vars into database configurations
   - Initializes Docker client
   - Sets up cron scheduler
   - Starts HTTP API server

2. **Backup Execution**:
   - Detects PostgreSQL version via SQL query
   - Creates temporary directory for dumps
   - Runs `pg_dumpall --roles-only` in Docker container
   - Runs `pg_dump --schema-only` in Docker container
   - Runs `pg_dump --data-only` in Docker container
   - Archives all files into `backup-*.tar.gz`
   - Moves archive and manifest to final location
   - Cleans up temporary files

3. **Scheduling**:
   - Uses `robfig/cron/v3` for scheduling
   - Supports 5-field cron expressions (minute hour day month weekday)
   - Automatically removes seconds field if 6-field format is provided
   - Runs in configured timezone

### Database Connection Parsing

Connection URLs are parsed in multiple places:

- **Initial parsing** (`internal/database/database.go`): Validates URL and extracts identifier
- **Backup operations** (`internal/backup/backup.go`): Re-parses to extract connection parameters for Docker containers

This duplication is intentional - the initial parse validates, while backup operations need individual parameters for environment variables passed to containers.

### File Structure

```
backups/
├── <project_name>/          # Lowercased project name from BACKUP_* env var
│   ├── YYYY-MM-DD/          # Date-based directories
│   │   ├── backup-<run_id>.tar.gz
│   │   └── manifest-<run_id>.json
│   └── ...
└── metadata/
    ├── latest.json          # Last backup run metadata
    └── running.json         # Current running status
```

### Metadata Storage

State is stored in JSON files in `metadata/` directory:

- **`latest.json`**: Contains full details of the last backup run (all databases, results, timestamps)
- **`running.json`**: Simple boolean flag indicating if a backup is currently running

This file-based approach:
- Survives service restarts
- No database required
- Easy to inspect/debug
- Atomic writes

## Docker Container Configuration

### Network Mode: Host

Backups use `container.NetworkMode("host")` because:

- External databases need to be reachable from inside containers
- Supabase connection poolers are external services
- Simplifies networking - no port mapping needed
- Works on Linux (default in Docker, macOS requires special setup)

**Note**: Host network mode doesn't work on macOS Docker Desktop by default. The service is primarily designed for Linux servers.

### Container Lifecycle

1. **Image Pulling**: Automatically pulls PostgreSQL image if not cached (Docker handles caching)
2. **Container Creation**: Creates temporary container with:
   - Matching PostgreSQL version image
   - Environment variables (PGHOST, PGPORT, PGUSER, PGPASSWORD)
   - Command: `pg_dump` or `pg_dumpall` with appropriate flags
   - Mount: Output directory bound to `/output`
3. **Execution**: Starts container, streams logs, waits for completion
4. **Cleanup**: Always removes container (via defer)

### Volume Mounts

Files are written via volume mounts:

- **Host directory**: Mounted to `/output` in container
- **Directory structure**: Container writes to `/output/<filename>`
- **Example**: `pg_dumpall --roles-only > /output/roles.sql`

The service mounts the **parent directory** of the target file, not the file itself (Docker volumes must be directories).

## Environment Variable Parsing

### BACKUP_* Prefix

- Scans all environment variables
- Looks for `BACKUP_` prefix (case-insensitive)
- Extracts project name: everything after `BACKUP_`
- Validates URL starts with `postgresql://`
- Lowercases project name for folder structure

**Examples**:
- `BACKUP_RUNNINGFOMO=postgresql://...` → project: `runningfomo`
- `BACKUP_MyProject=postgresql://...` → project: `myproject`

### Special Characters in Passwords

Connection URLs may contain special characters in passwords (`@`, `:`, `/`, etc.). The service:

- Uses `url.Parse()` for initial validation
- Extracts components and passes them as environment variables to Docker containers
- Avoids issues with URL-encoded passwords
- Uses `PGPASSWORD` environment variable (standard PostgreSQL approach)

## Error Handling

### Backup Failures

When a backup fails:

1. **Manifest created**: Failed manifest includes error message
2. **Files preserved**: Failed backups don't create archives, but metadata is saved
3. **Status tracking**: `latest.json` includes failure details
4. **Logging**: Errors logged with context (database name, step that failed)

### Docker Failures

- **Container exit codes**: Non-zero exit codes return errors with stderr output
- **Image pull failures**: Returns error immediately (doesn't retry)
- **Socket access**: Checked at startup - service won't start if Docker unavailable

## Postgres Version Detection

### How It Works

1. Connects to database using `pgx`
2. Queries: `SELECT version()`
3. Parses result: `PostgreSQL 17.1` → extracts major version `17`
4. Falls back to version `17` if parsing fails

### Version Matching

- Uses `postgres:<major_version>` Docker image
- Example: Database version `17.1` → uses `postgres:17`
- Ensures `pg_dump` version matches server version

## Backup Process Details

### Three-Phase Dump

1. **Roles** (`pg_dumpall --roles-only`):
   - Dumps all PostgreSQL roles and permissions
   - Required for full database restoration
   - Runs against `postgres` database (roles are cluster-wide)

2. **Schema** (`pg_dump --schema-only`):
   - Dumps table definitions, views, functions, triggers
   - Uses `--no-owner --no-acl --no-privileges` flags
   - Results in portable schema that doesn't require specific roles

3. **Data** (`pg_dump --data-only`):
   - Dumps all table data
   - Uses `--column-inserts` for portable INSERT statements
   - Uses `--use-set-session-authorization` for compatibility

### Archive Creation

- All three SQL files are archived into a single `tar.gz` file
- Archive includes: `roles.sql`, `schema.sql`, `data.sql`
- Manifest JSON is saved separately (not in archive)
- Archive naming: `backup-<project>-<date>-<time>.tar.gz`

## Retention Cleanup

### How It Works

- Runs after each backup job completes
- Scans date-based directories in each project folder
- Compares directory names (format: `YYYY-MM-DD`) with cutoff date
- Removes directories older than `RETENTION_DAYS`
- Operates on entire date directories (not individual files)

### Retention Logic

```go
cutoffDate := time.Now().AddDate(0, 0, -retentionDays)
// Directories with names < cutoffDate (as strings) are deleted
```

This works because ISO date format (`YYYY-MM-DD`) is lexicographically sortable.

## CLI Communication

### HTTP API Over Unix Socket (Removed)

Originally supported Unix socket communication, now uses HTTP only:

- **Old approach**: CLI connected via `/tmp/backup-service.sock`
- **Current approach**: CLI uses HTTP API at `http://localhost:8080`
- **Environment variable**: `API_URL` can override default

### CLI Commands

- `status`: GET `/status` - Returns service status and last run info
- `backup <project>`: POST `/run/<project>` - Triggers backup for specific project

Both return JSON responses that CLI formats for display.

## Testing

### Integration Test

The `integration_test.go` file:

- Builds binaries dynamically
- Starts backup service as subprocess
- Waits for service to be ready (health check)
- Tests CLI commands
- Triggers backup and waits for completion
- Verifies backup files exist
- Cleans up test artifacts

**Requirements**:
- Docker must be available (for backup operations)
- PostgreSQL service must be running (GitHub Actions provides this)
- `TEST_DB_URL` environment variable (optional, defaults to localhost)

### Running Tests

```bash
# Unit tests (excludes integration)
go test ./... -v -run '^Test[^I]'

# Integration test only
go test -v -run '^TestIntegration$' -timeout 10m

# All tests
go test ./... -v
```

## Known Limitations

### macOS Compatibility

- Host network mode doesn't work on Docker Desktop for macOS
- Service should run on Linux servers or use Docker-in-Docker
- For local macOS development, may need to use bridge network mode

### Concurrent Backups

- Backups run sequentially (no parallelization)
- If a backup is running, new backup requests wait or fail
- File-based locking prevents race conditions

### Database Size

- No compression of individual SQL files (only tar.gz archive)
- Large databases may produce large backup files
- No streaming/chunking for very large databases

### Network Requirements

- Containers need network access to PostgreSQL databases
- If using host network mode, firewall rules must allow connections
- Connection poolers (like Supabase) are fully supported

## Troubleshooting

### "Container exited with code 1"

- Check Docker logs for the specific error
- Verify database credentials
- Check network connectivity from container to database
- Ensure PostgreSQL version detection is correct

### "Docker daemon is not accessible"

- Verify Docker is running: `docker ps`
- Check socket permissions: `ls -l /var/run/docker.sock`
- Ensure socket is mounted in Docker Compose
- On macOS, may need Docker Desktop running

### "No databases configured"

- Check environment variables have `BACKUP_` prefix
- Verify URLs start with `postgresql://`
- Check for typos in variable names
- Logs will show which variables were parsed

### Backup Files Not Created

- Check backup directory permissions
- Verify `LOCAL_BACKUP_DIR` is set correctly
- Check disk space
- Review service logs for errors during archive creation

### Version Mismatch Errors

- Service auto-detects version, but can be manually verified
- Check database version: `SELECT version();`
- Verify Docker image exists: `docker pull postgres:17`
- Service falls back to version 17 if detection fails

## Development

### Local Setup

```bash
# Build
go build -o backup ./cmd/backup
go build -o cli ./cmd/cli

# Run service
export BACKUP_TESTDB=postgresql://user:pass@localhost:5432/db
export LOCAL_BACKUP_DIR=./backups
./backup

# In another terminal, test CLI
export API_URL=http://localhost:8080
./cli status
./cli backup testdb
```

### Debugging

- Set `LOG_LEVEL=DEBUG` for verbose logging
- Use `LOG_FORMAT=text` for human-readable logs
- Check `metadata/latest.json` for last run details
- Check `metadata/running.json` for current status

### Code Structure

```
cmd/
  backup/        # Main service entry point
  cli/           # CLI tool entry point
internal/
  api/           # HTTP API server
  backup/        # Backup execution logic
  config/        # Configuration loading
  database/      # Database connection parsing
  docker/        # Docker client wrapper
  metadata/      # File-based state management
  retention/     # Cleanup logic
  service/       # Main orchestration logic
```

## Future Considerations

### Potential Enhancements

- **Parallel backups**: Could use goroutines with semaphore for concurrency
- **Compression options**: Could add per-file compression or different algorithms
- **Backup verification**: Could restore to temporary database to verify
- **S3/storage backends**: Could add remote storage support
- **Webhook notifications**: Could notify on backup completion/failure
- **Backup encryption**: Could encrypt archives at rest

### Maintenance Notes

- Go version: Currently 1.22 (updated from incorrect 1.24.0)
- Docker API: Uses v25 (latest at time of writing)
- PostgreSQL: Supports versions 12+ (tested with 17)
- Dependencies: Managed via `go.mod`, minimal external deps

