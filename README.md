# pg-backup-scheduler

PostgreSQL Backup Scheduler - A minimal, self-hosted backup service for PostgreSQL databases. Perfect for Supabase users who want automated backups without paying for premium features.

Uses Docker containers with matching PostgreSQL versions (like Supabase CLI) to ensure compatibility. Backups run daily via cron and are stored locally with automatic retention cleanup.

**Built with Go** - single binary, no dependencies beyond Docker.

## Quick Start

1. Clone and configure:

```bash
git clone https://github.com/mxschmitt/pg-backup-scheduler.git
cd pg-backup-scheduler
cp env.example .env
```

2. Add your database URLs to `.env`:

```bash
# Format: BACKUP_<PROJECT_NAME>=postgresql://user:password@host:port/database
BACKUP_RUNNINGFOMO=postgresql://postgres.abcdefgh:password@aws-1-eu-north-1.pooler.supabase.com:5432/postgres
BACKUP_STRIDE=postgresql://postgres:password@localhost:5432/stride_db
```

The project name (after `BACKUP_`) is lowercased and used as the backup folder name.

3. Start:

```bash
docker compose up -d
```

Backups run daily at 00:30 (Europe/Berlin, configurable via `BACKUP_CRON`).

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `BACKUP_*` | - | Database URLs (prefix with `BACKUP_` + project name) |
| `RETENTION_DAYS` | `30` | Number of days to keep backups |
| `BACKUP_CRON` | `30 0 * * *` | Cron expression for backup schedule |
| `TZ` | `Europe/Berlin` | Timezone for scheduling |
| `LOCAL_BACKUP_DIR` | `./backups` | Local path for backups (use `/data/backups` in Docker) |
| `SERVICE_PORT` | `8080` | HTTP API port |
| `LOG_LEVEL` | `INFO` | Log level (DEBUG, INFO, WARN, ERROR) |
| `LOG_FORMAT` | `json` | Log format (json or text) |

## Usage

### Check Status

```bash
# Via HTTP API
curl http://localhost:8080/status | jq

# Via CLI tool (runs inside the container)
docker compose exec backup-service cli status
```

### Trigger Manual Backup

```bash
# Backup all databases
curl -X POST http://localhost:8080/run

# Backup specific project
curl -X POST http://localhost:8080/run/runningfomo

# Or via CLI (runs inside the container)
docker compose exec backup-service cli backup runningfomo
```

### API Endpoints

- `GET /healthz` - Health check
- `GET /readyz` - Readiness probe
- `GET /status` - Service status and last run info
- `POST /run` - Trigger backup for all databases
- `POST /run/{project}` - Trigger backup for specific project

## Backup Format

Backups are stored in `backups/<project_name>/YYYY-MM-DD/` and contain:

1. **backup-*.tar.gz** - Archive with roles, schema, and data
2. **manifest-*.json** - Backup metadata (timestamps, status, PostgreSQL version, database size)

The archive contains three SQL files:
- `roles.sql` - PostgreSQL roles and permissions
- `schema.sql` - Database schema
- `data.sql` - Data dump

## Restore

```bash
# Extract backup
cd backups/runningfomo/2026-01-07
tar -xzf backup-*.tar.gz

# Restore (in order)
psql $TARGET_DB_URL < roles.sql
psql $TARGET_DB_URL < schema.sql
psql $TARGET_DB_URL < data.sql
```

## How It Works

- Auto-detects PostgreSQL version for each database
- Uses matching Docker container (e.g., `postgres:17`) to run `pg_dump`/`pg_dumpall`
- Creates tar.gz archive with roles, schema, and data
- Stores backups locally with automatic retention cleanup
- Runs on schedule via cron (default: daily at 00:30)

## Requirements

- Docker (socket mounted at `/var/run/docker.sock`)
- PostgreSQL connection strings (works with Supabase connection pooler)
- Disk space for backups (configurable retention)

## Acknowledgments

This project was inspired by and borrows concepts from the [Supabase CLI](https://github.com/supabase/cli) repository, particularly the approach of using Docker containers with matching PostgreSQL versions for database operations.

## License

MIT
