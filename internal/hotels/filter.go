package hotels

// HotelFilter narrows the public hotel list (from MaynStay's hotels/dto.go).
type HotelFilter struct {
	Category string
	City     string
	Search   string
	MinPrice float64
	MaxPrice float64
	Sort     string
	OwnerID  int64
	IsActive *bool
}
