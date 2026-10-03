package docker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Compose labels Docker Compose puts on every container it creates.
const (
	LabelProject     = "com.docker.compose.project"
	LabelWorkingDir  = "com.docker.compose.project.working_dir"
	LabelConfigFiles = "com.docker.compose.project.config_files"
	LabelService     = "com.docker.compose.service"
)

// Dump kinds.
const (
	DumpPostgres = "postgres"
	DumpMariaDB  = "mariadb"
	DumpRedis    = "redis"
)

type Stack struct {
	Project     string
	WorkingDir  string
	ConfigFiles []string
	Services    []Service
}

type Service struct {
	Name        string
	ContainerID string
	Container   string
	Image       string
	State       string
	Mounts      []Mount // bind mounts and volumes with a host path
	DumpKind    string  // suggested database dump, "" if none
	SQLiteFiles []string
}

// Stacks groups containers into compose projects. Containers without
// compose labels are ignored.
func (c *Client) Stacks(ctx context.Context) ([]Stack, error) {
	containers, err := c.Containers(ctx)
	if err != nil {
		return nil, err
	}
	byProject := map[string]*Stack{}
	for _, ct := range containers {
		project := ct.Labels[LabelProject]
		if project == "" {
			continue
		}
		st := byProject[project]
		if st == nil {
			st = &Stack{Project: project, WorkingDir: ct.Labels[LabelWorkingDir]}
			for _, f := range strings.Split(ct.Labels[LabelConfigFiles], ",") {
				if f = strings.TrimSpace(f); f != "" {
					st.ConfigFiles = append(st.ConfigFiles, f)
				}
			}
			byProject[project] = st
		}
		svc := Service{
			Name:        ct.Labels[LabelService],
			ContainerID: ct.ID,
			Container:   ct.Name(),
			Image:       ct.Image,
			State:       ct.State,
			DumpKind:    DetectDump(ct.Image),
		}
		for _, m := range ct.Mounts {
			if (m.Type == "bind" || m.Type == "volume") && m.Source != "" {
				svc.Mounts = append(svc.Mounts, m)
				svc.SQLiteFiles = append(svc.SQLiteFiles, findSQLite(m.Source, 3, 5)...)
			}
		}
		st.Services = append(st.Services, svc)
	}
	out := make([]Stack, 0, len(byProject))
	for _, st := range byProject {
		sort.Slice(st.Services, func(i, j int) bool { return st.Services[i].Name < st.Services[j].Name })
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}

// FindStack returns the named compose project.
func (c *Client) FindStack(ctx context.Context, project string) (Stack, bool, error) {
	stacks, err := c.Stacks(ctx)
	if err != nil {
		return Stack{}, false, err
	}
	for _, s := range stacks {
		if s.Project == project {
			return s, true, nil
		}
	}
	return Stack{}, false, nil
}

// DetectDump guesses the dump strategy from an image name.
func DetectDump(image string) string {
	img := strings.ToLower(image)
	if i := strings.LastIndex(img, "/"); i >= 0 {
		img = img[i+1:]
	}
	switch {
	case strings.Contains(img, "postgres"), strings.Contains(img, "pgvecto"), strings.Contains(img, "postgis"):
		return DumpPostgres
	case strings.Contains(img, "mariadb"), strings.Contains(img, "mysql"):
		return DumpMariaDB
	case strings.Contains(img, "valkey"), strings.Contains(img, "redis"):
		return DumpRedis
	}
	return ""
}

// findSQLite looks a few levels deep for SQLite databases, which need the
// app paused (or stopped) for a consistent copy.
func findSQLite(root string, depth, limit int) []string {
	var out []string
	base := strings.Count(filepath.Clean(root), string(filepath.Separator))
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // the callback skips unreadable entries; a partial result is intended
		if err != nil || len(out) >= limit {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if strings.Count(p, string(filepath.Separator))-base >= depth {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".db", ".sqlite", ".sqlite3":
			if isSQLite(p) {
				out = append(out, p)
			}
		}
		return nil
	})
	return out
}

func isSQLite(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck // read-only file
	hdr := make([]byte, 16)
	n, _ := f.Read(hdr)
	return n == 16 && string(hdr) == "SQLite format 3\x00"
}

// DumpCommand is the command run inside a database container to write a
// full dump to stdout, and the dump file's extension. Credentials come from
// the container's own environment.
func DumpCommand(kind string) ([]string, string, bool) {
	switch kind {
	case DumpPostgres:
		return []string{"sh", "-c", `exec pg_dumpall -U "${POSTGRES_USER:-postgres}"`}, ".sql", true
	case DumpMariaDB:
		return []string{"sh", "-c", `P="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; ` +
			`D=$(command -v mariadb-dump || command -v mysqldump); ` +
			`MYSQL_PWD="$P" exec "$D" -uroot --all-databases --single-transaction --routines --events --triggers`}, ".sql", true
	case DumpRedis:
		return []string{"sh", "-c", `C=$(command -v valkey-cli || command -v redis-cli); ` +
			`P="${REDIS_PASSWORD:-${VALKEY_PASSWORD:-}}"; if [ -n "$P" ]; then export REDISCLI_AUTH="$P"; fi; ` +
			`exec "$C" --rdb -`}, ".rdb", true
	}
	return nil, "", false
}

// ImportCommand reads a dump from stdin into a running database container.
// Redis/Valkey dumps can't be loaded this way (the RDB file has to replace
// the data file while the server is stopped).
func ImportCommand(kind string) ([]string, bool) {
	switch kind {
	case DumpPostgres:
		return []string{"sh", "-c", `exec psql -q -U "${POSTGRES_USER:-postgres}" -d postgres`}, true
	case DumpMariaDB:
		return []string{"sh", "-c", `P="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; ` +
			`C=$(command -v mariadb || command -v mysql); MYSQL_PWD="$P" exec "$C" -uroot`}, true
	}
	return nil, false
}
