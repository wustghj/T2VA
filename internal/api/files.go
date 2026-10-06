package api

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"t2va/internal/identity"
)

const (
	fileInput     = "input"
	fileResult    = "result"
	fileOwned     = "owned"
	fileUnclaimed = "unclaimed"
)

type File struct {
	ID        string
	OwnerType identity.PrincipalType
	OwnerID   string
	Kind      string
	Status    string
	TaskID    string
	CreatedAt time.Time
}

type FileRepository struct{ db *sql.DB }

func NewFileRepository(db *sql.DB) *FileRepository { return &FileRepository{db: db} }

func (r *FileRepository) RegisterOwned(ctx context.Context, id string, p identity.Principal, kind string) error {
	if p.ID == "" || (p.Type != identity.User && p.Type != identity.Guest) || kind != fileInput {
		return errors.New("invalid file owner or kind")
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO files(id,owner_type,owner_id,kind,status,created_at) VALUES(?,?,?,?,?,?)`, id, p.Type, p.ID, kind, fileOwned, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (r *FileRepository) RegisterUnclaimedResult(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO files(id,owner_type,owner_id,kind,status,created_at) VALUES(?,'','','result',?,?)`, id, fileUnclaimed, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (r *FileRepository) GetOwned(ctx context.Context, id string, p identity.Principal) (File, error) {
	if (p.Type != identity.User && p.Type != identity.Guest) || p.ID == "" {
		return File{}, errors.New("file not found")
	}
	var f File
	var ownerType string
	var created string
	err := r.db.QueryRowContext(ctx, `SELECT id,owner_type,owner_id,kind,status,task_id,created_at FROM files WHERE id=? AND owner_type=? AND owner_id=? AND status=?`, id, p.Type, p.ID, fileOwned).Scan(&f.ID, &ownerType, &f.OwnerID, &f.Kind, &f.Status, &f.TaskID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, errors.New("file not found")
	}
	if err != nil {
		return File{}, err
	}
	f.OwnerType = identity.PrincipalType(ownerType)
	f.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return f, err
}

func (r *FileRepository) GetWorkerInput(ctx context.Context, id string) (File, error) {
	var f File
	var ownerType string
	var created string
	err := r.db.QueryRowContext(ctx, `SELECT id,owner_type,owner_id,kind,status,task_id,created_at FROM files WHERE id=? AND kind=? AND status=?`, id, fileInput, fileOwned).Scan(&f.ID, &ownerType, &f.OwnerID, &f.Kind, &f.Status, &f.TaskID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, errors.New("file not found")
	}
	if err != nil {
		return File{}, err
	}
	f.OwnerType = identity.PrincipalType(ownerType)
	f.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return f, err
}
