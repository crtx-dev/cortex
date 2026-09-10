package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	corelauncher "github.com/gantry-tools/gantry-core/launcher"
)

func (a *App) launcherRoot(static http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Has("config") {
			if !a.authenticated(r) {
				corelauncher.WriteAccessError(w, http.StatusUnauthorized, "Cortex", "C", "#7fc89b")
				return
			}
			a.serveLauncher(static, w, r)
			return
		}
		instances, err := a.loadLauncherInstances(r.Context())
		if err != nil {
			http.Error(w, "launcher unavailable", http.StatusInternalServerError)
			return
		}
		switch len(instances) {
		case 0:
			http.Redirect(w, r, "/app/", http.StatusFound)
		case 1:
			target, err := corelauncher.AppURL(instances[0])
			if err != nil {
				http.Error(w, "invalid launcher configuration", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, target, http.StatusFound)
		default:
			a.serveLauncher(static, w, r)
		}
	}
}

func (a *App) serveLauncher(static http.Handler, w http.ResponseWriter, r *http.Request) {
	clone := r.Clone(r.Context())
	clone.URL.Path, clone.URL.RawPath = "/launcher.html", ""
	w.Header().Set("Cache-Control", "no-store")
	static.ServeHTTP(w, clone)
}

func (a *App) launcherInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := a.loadLauncherInstances(r.Context())
	if err != nil {
		http.Error(w, "launcher unavailable", http.StatusInternalServerError)
		return
	}
	view, err := corelauncher.MakeView("cortex", instances)
	if err != nil {
		http.Error(w, "invalid launcher configuration", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonOut(w, view)
}

func (a *App) launcherConfig(w http.ResponseWriter, r *http.Request) {
	var document corelauncher.Document
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	instances, err := corelauncher.Normalize("cortex", document)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.replaceLauncherInstances(r.Context(), instances); err != nil {
		http.Error(w, "unable to save launcher configuration", http.StatusInternalServerError)
		return
	}
	view, _ := corelauncher.MakeView("cortex", instances)
	jsonOut(w, view)
}

func (a *App) loadLauncherInstances(ctx context.Context) ([]corelauncher.Instance, error) {
	rows, err := a.db.QueryContext(ctx, "SELECT id,name,domain,port FROM launcher_instances ORDER BY position,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	instances := []corelauncher.Instance{}
	for rows.Next() {
		var item corelauncher.Instance
		if err := rows.Scan(&item.ID, &item.Name, &item.Domain, &item.Port); err != nil {
			return nil, err
		}
		instances = append(instances, item)
	}
	return instances, rows.Err()
}

func (a *App) replaceLauncherInstances(ctx context.Context, instances []corelauncher.Instance) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM launcher_instances"); err != nil {
		return err
	}
	for position, item := range instances {
		if _, err := tx.ExecContext(ctx, "INSERT INTO launcher_instances(id,position,name,domain,port) VALUES(?,?,?,?,?)", item.ID, position, item.Name, item.Domain, item.Port); err != nil {
			return fmt.Errorf("save launcher instance: %w", err)
		}
	}
	return tx.Commit()
}
