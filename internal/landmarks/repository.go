package landmarks

import (
	"context"
	"database/sql"
)

// Repository is the read-only landmark lookup the chatbot uses for "near"
// and distance questions (adapted from MaynStay's internal/landmarks; the
// admin create/update/delete paths are not part of this backend).
type Repository interface {
	ListByCity(ctx context.Context, city string) ([]Landmark, error)
}

type repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) Repository {
	return &repository{db: db}
}

const landmarkColumns = `landmark_id, city, name, COALESCE(category, ''), latitude, longitude, is_active, created_at`

func (r *repository) ListByCity(ctx context.Context, city string) ([]Landmark, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT "+landmarkColumns+" FROM landmarks WHERE city = ? AND is_active = 1 ORDER BY name ASC", city)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Landmark{}
	for rows.Next() {
		var l Landmark
		if err := rows.Scan(&l.LandmarkID, &l.City, &l.Name, &l.Category, &l.Latitude, &l.Longitude, &l.IsActive, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Service satisfies the chatbot's LandmarkSource.
type Service interface {
	ListByCity(ctx context.Context, city string) ([]Landmark, error)
}

type service struct{ repo Repository }

func NewService(repo Repository) Service { return &service{repo: repo} }

func (s *service) ListByCity(ctx context.Context, city string) ([]Landmark, error) {
	return s.repo.ListByCity(ctx, city)
}
