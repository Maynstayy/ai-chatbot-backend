package hotels

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/Maynstayy/ai-chatbot-backend/internal/gst"
)

// Repository is the read-only hotel catalogue (adapted from MaynStay's
// internal/hotels: the create/update/delete/owner paths are not part of the
// standalone chatbot backend).
type Repository interface {
	GetHotels(ctx context.Context, filter HotelFilter) ([]Hotel, error)
	GetHotelByID(ctx context.Context, id int64) (*Hotel, error)
}

type repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) Repository {
	return &repository{db: db}
}

// startingPrice is the hotel's "from" price: the cheapest room type that
// actually has rooms. The stored hotels.price_per_night is only a fallback
// for a hotel with no rooms yet - reading it directly let the listing price
// drift from the real room prices whenever a room was repriced.
const startingPrice = `COALESCE((
		SELECT MIN(rt.price_per_night) FROM room_types rt
		WHERE rt.hotel_id = hotels.hotel_id AND rt.price_per_night > 0
		  AND EXISTS (SELECT 1 FROM hotel_rooms hr WHERE hr.room_type_id = rt.room_type_id)
	), hotels.price_per_night)`

const hotelColumns = `
	hotel_id, name, location, description, city, state, country, address,
	latitude, longitude,
	category, ` + startingPrice + ` AS price_per_night, rating, total_reviews, amenities, thumbnail_url, images,
	owner_id, is_active, is_deleted, free_cancellation, pay_at_property, instant_booking,
	cancellation_hours, child_policy, pet_policy, id_rules, COALESCE(gstin, ''),
	created_at, updated_at`

func (r *repository) GetHotels(ctx context.Context, filter HotelFilter) ([]Hotel, error) {
	query := "SELECT " + hotelColumns + " FROM hotels"

	conditions := []string{"is_deleted = 0"}
	args := []interface{}{}

	if filter.Category != "" {
		conditions = append(conditions, "category = ?")
		args = append(args, filter.Category)
	}
	if filter.City != "" {
		conditions = append(conditions, "city = ?")
		args = append(args, filter.City)
	}
	if filter.Search != "" {
		conditions = append(conditions, "(name LIKE ? OR location LIKE ?)")
		args = append(args, "%"+filter.Search+"%", "%"+filter.Search+"%")
	}
	if filter.MinPrice > 0 {
		conditions = append(conditions, startingPrice+" >= ?")
		args = append(args, filter.MinPrice)
	}
	if filter.MaxPrice > 0 {
		conditions = append(conditions, startingPrice+" <= ?")
		args = append(args, filter.MaxPrice)
	}
	if filter.OwnerID > 0 {
		conditions = append(conditions, "owner_id = ?")
		args = append(args, filter.OwnerID)
	}
	if filter.IsActive != nil {
		conditions = append(conditions, "is_active = ?")
		args = append(args, *filter.IsActive)
	}

	query += " WHERE " + strings.Join(conditions, " AND ")

	switch filter.Sort {
	case "price_asc":
		query += " ORDER BY price_per_night ASC"
	case "price_desc":
		query += " ORDER BY price_per_night DESC"
	case "rating":
		query += " ORDER BY rating DESC"
	default:
		query += " ORDER BY created_at DESC"
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Hotel
	for rows.Next() {
		h, err := scanHotel(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		result = []Hotel{}
	}
	return result, nil
}

// GetHotelByID enriches the base row with two cheap extra lookups that only
// matter for the single-property detail page, not the list endpoint: the
// owning vendor's tax rate / check-in / check-out (vendor_settings is
// per-account, so a multi-property vendor currently shares one value across
// all their hotels), and a real count of bookings placed in the last 24h.
// GetHotelByID returns one listed hotel. In MaynStay the check-in/check-out
// times and tax rate came from the owning vendor's settings and a 24-hour
// booking count was added; the standalone catalogue stores the times on the
// hotel itself and has no bookings.
func (r *repository) GetHotelByID(ctx context.Context, id int64) (*Hotel, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT "+hotelColumns+" FROM hotels WHERE hotel_id = ? AND is_deleted = 0", id)
	hotel, err := scanHotel(row)
	if err != nil {
		return nil, err
	}

	var checkIn, checkOut sql.NullString
	var taxRate sql.NullFloat64
	err = r.db.QueryRowContext(ctx,
		"SELECT check_in_time, check_out_time, tax_rate FROM hotels WHERE hotel_id = ?", id,
	).Scan(&checkIn, &checkOut, &taxRate)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	hotel.TaxRate = taxRate.Float64
	hotel.CheckInTime = orDefault(checkIn, "14:00")
	hotel.CheckOutTime = orDefault(checkOut, "12:00")
	return hotel, nil
}

func orDefault(v sql.NullString, fallback string) string {
	if v.Valid && v.String != "" {
		return v.String
	}
	return fallback
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanHotel(row rowScanner) (*Hotel, error) {
	var h Hotel
	var (
		state, country, address, category, thumbnailURL sql.NullString
		amenitiesRaw, imagesRaw                         sql.NullString
		latitude, longitude                             sql.NullFloat64
		price, rating                                   sql.NullFloat64
		totalReviews                                    sql.NullInt64
		ownerID                                         sql.NullInt64
		isActive, isDeleted                             sql.NullBool
		freeCancellation, payAtProperty, instantBooking sql.NullBool
		cancellationHours                               sql.NullInt64
		childPolicy, petPolicy, idRules, gstin          sql.NullString
	)

	err := row.Scan(
		&h.HotelID, &h.Name, &h.Location, &h.Description, &h.City, &state,
		&country, &address, &latitude, &longitude, &category, &price, &rating,
		&totalReviews, &amenitiesRaw, &thumbnailURL, &imagesRaw, &ownerID,
		&isActive, &isDeleted, &freeCancellation, &payAtProperty, &instantBooking,
		&cancellationHours, &childPolicy, &petPolicy, &idRules, &gstin,
		&h.CreatedAt, &h.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if cancellationHours.Valid {
		v := int(cancellationHours.Int64)
		h.CancellationHours = &v
	}
	h.ChildPolicy = childPolicy.String
	h.PetPolicy = petPolicy.String
	h.IDRules = idRules.String
	h.GSTIN = gstin.String

	h.State = state.String
	h.Country = country.String
	h.Address = address.String
	if latitude.Valid {
		h.Latitude = &latitude.Float64
	}
	if longitude.Valid {
		h.Longitude = &longitude.Float64
	}
	h.Category = category.String
	h.ThumbnailURL = thumbnailURL.String
	h.PricePerNight = price.Float64
	h.GSTRate = gst.Rate(h.PricePerNight)
	h.TaxPerNight = gst.TaxPerNight(h.PricePerNight)
	h.Rating = rating.Float64
	h.TotalReviews = int(totalReviews.Int64)
	h.OwnerID = ownerID.Int64
	h.IsActive = isActive.Bool
	h.IsDeleted = isDeleted.Bool
	h.FreeCancellation = freeCancellation.Bool
	h.PayAtProperty = payAtProperty.Bool
	h.InstantBooking = instantBooking.Bool

	h.Amenities, err = unmarshalStringArray(amenitiesRaw)
	if err != nil {
		return nil, err
	}
	h.Images, err = unmarshalStringArray(imagesRaw)
	if err != nil {
		return nil, err
	}

	return &h, nil
}

func unmarshalStringArray(raw sql.NullString) ([]string, error) {
	if !raw.Valid || raw.String == "" {
		return []string{}, nil
	}
	var amenities []string
	if err := json.Unmarshal([]byte(raw.String), &amenities); err != nil {
		return nil, err
	}
	return amenities, nil
}
