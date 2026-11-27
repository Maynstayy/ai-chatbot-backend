package hotels

import "context"

// Service is the read-only public hotel catalogue. It satisfies the
// chatbot's HotelSource (internal/chatbot/tools.go).
type Service interface {
	GetHotels(ctx context.Context, filter HotelFilter) ([]Hotel, error)
	GetHotelByID(ctx context.Context, id int64) (*Hotel, error)
}

type service struct {
	repo Repository
}

func NewService(repo Repository) Service {
	return &service{repo: repo}
}

func (s *service) GetHotels(ctx context.Context, filter HotelFilter) ([]Hotel, error) {
	return s.repo.GetHotels(ctx, filter)
}

func (s *service) GetHotelByID(ctx context.Context, id int64) (*Hotel, error) {
	return s.repo.GetHotelByID(ctx, id)
}
