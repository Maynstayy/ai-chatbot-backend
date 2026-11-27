package roomtypes

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Maynstayy/ai-chatbot-backend/internal/rooms"
)

type Repository interface {
	GetRoomTypesByHotelID(ctx context.Context, hotelID int64, checkIn, checkOut *string) ([]RoomType, error)
}

type repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) Repository {
	return &repository{db: db}
}

const roomTypeColumns = `
	rt.room_type_id, rt.hotel_id, h.name, rt.name, rt.capacity, rt.price_per_night,
	COALESCE(rt.description, ''), rt.amenities, rt.images,
	COALESCE(rt.bed_type, ''), rt.size_sqm, COALESCE(rt.view_type, ''),
	COUNT(r.room_id) AS total_rooms,
	SUM(CASE WHEN r.status = 'Available' THEN 1 ELSE 0 END) AS available_rooms,
	MIN(CASE WHEN r.status = 'Available' THEN r.room_id END) AS sample_room_id`

// GetRoomTypesByHotelID returns one row per room type for the hotel, with
// AvailableCount computed as: physical rooms currently marked Available,
// minus any overlapping confirmed/pending bookings for that type in the
// given date range. When checkIn/checkOut are nil, no date filter is applied
// and AvailableCount is just the raw count of Available physical rooms.
func (r *repository) GetRoomTypesByHotelID(ctx context.Context, hotelID int64, checkIn, checkOut *string) ([]RoomType, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+roomTypeColumns+`
		FROM room_types rt
		JOIN hotels h ON rt.hotel_id = h.hotel_id
		LEFT JOIN hotel_rooms r ON r.room_type_id = rt.room_type_id
		WHERE rt.hotel_id = ?
		GROUP BY rt.room_type_id
		ORDER BY rt.price_per_night ASC`,
		hotelID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RoomType
	sampleRoomIDs := make(map[int64]int64) // room_type_id -> a representative Available room_id
	for rows.Next() {
		var rt RoomType
		var amenitiesRaw, imagesRaw sql.NullString
		var sizeSqm sql.NullInt64
		var availableRooms sql.NullInt64
		var sampleRoomID sql.NullInt64
		if err := rows.Scan(
			&rt.RoomTypeID, &rt.HotelID, &rt.HotelName, &rt.Name, &rt.Capacity, &rt.PricePerNight,
			&rt.Description, &amenitiesRaw, &imagesRaw,
			&rt.BedType, &sizeSqm, &rt.ViewType,
			&rt.TotalRooms, &availableRooms, &sampleRoomID,
		); err != nil {
			return nil, err
		}
		if sizeSqm.Valid {
			v := int(sizeSqm.Int64)
			rt.SizeSqm = &v
		}
		rt.Amenities, err = unmarshalStringArray(amenitiesRaw)
		if err != nil {
			return nil, err
		}
		rt.Images, err = unmarshalStringArray(imagesRaw)
		if err != nil {
			return nil, err
		}
		if availableRooms.Valid {
			rt.AvailableCount = int(availableRooms.Int64)
		}
		if sampleRoomID.Valid {
			sampleRoomIDs[rt.RoomTypeID] = sampleRoomID.Int64
		}
		result = append(result, rt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		result = []RoomType{}
	}

	if checkIn != nil && checkOut != nil {
		if err := r.applyDateOverlap(ctx, result, *checkIn, *checkOut); err != nil {
			return nil, err
		}
	}

	if err := r.attachRatePlans(ctx, result, sampleRoomIDs); err != nil {
		return nil, err
	}
	return result, nil
}

// applyDateOverlap subtracts, per room type, the number of rooms already
// held for overlapping stays (pending or confirmed) in the given date range -
// a room_count of 3 on one overlapping hold counts as 3 rooms unavailable,
// not 1. The standalone catalogue has no booking system: room_holds stands in
// for MaynStay's bookings table with the same columns this query reads.
func (r *repository) applyDateOverlap(ctx context.Context, types []RoomType, checkIn, checkOut string) error {
	if len(types) == 0 {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT room_type_id, COALESCE(SUM(room_count), 0)
		FROM room_holds
		WHERE room_type_id IS NOT NULL
		  AND status IN ('pending', 'confirmed')
		  AND check_in < ? AND check_out > ?
		GROUP BY room_type_id`,
		checkOut, checkIn,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	booked := make(map[int64]int)
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return err
		}
		booked[id] = count
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range types {
		if n := booked[types[i].RoomTypeID]; n > 0 {
			types[i].AvailableCount -= n
			if types[i].AvailableCount < 0 {
				types[i].AvailableCount = 0
			}
		}
	}
	return nil
}

// attachRatePlans loads active rate plans from one representative Available
// room per type (sampleRoomIDs) - rate plans are still stored per physical
// room, so this is the type-level view of "what a guest booking this type
// would see", accepting that divergent per-room rate plans within a type
// collapse to one room's set (documented known limitation).
func (r *repository) attachRatePlans(ctx context.Context, types []RoomType, sampleRoomIDs map[int64]int64) error {
	if len(sampleRoomIDs) == 0 {
		return nil
	}
	ids := make([]interface{}, 0, len(sampleRoomIDs))
	roomToType := make(map[int64]int64, len(sampleRoomIDs))
	for typeID, roomID := range sampleRoomIDs {
		ids = append(ids, roomID)
		roomToType[roomID] = typeID
	}
	placeholders := ""
	for i := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT rate_plan_id, room_id, name, description, price_per_night, max_occupancy,
		       free_cancellation, pay_at_property, breakfast_included, inclusions, is_active, created_at, updated_at,
		       discount_type, discount_value, discount_valid_from, discount_valid_to, rooms_left
		FROM room_rate_plans
		WHERE room_id IN (`+placeholders+`) AND is_active = 1
		ORDER BY price_per_night ASC`,
		ids...,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	plansByType := make(map[int64][]rooms.RatePlan)
	for rows.Next() {
		var rp rooms.RatePlan
		var inclusionsRaw sql.NullString
		var validFrom, validTo sql.NullString
		var roomsLeft sql.NullInt64
		if err := rows.Scan(
			&rp.RatePlanID, &rp.RoomID, &rp.Name, &rp.Description, &rp.PricePerNight, &rp.MaxOccupancy,
			&rp.FreeCancellation, &rp.PayAtProperty, &rp.BreakfastIncluded,
			&inclusionsRaw, &rp.IsActive, &rp.CreatedAt, &rp.UpdatedAt,
			&rp.DiscountType, &rp.DiscountValue, &validFrom, &validTo, &roomsLeft,
		); err != nil {
			return err
		}
		rp.Inclusions, err = unmarshalStringArray(inclusionsRaw)
		if err != nil {
			return err
		}
		if validFrom.Valid {
			rp.DiscountValidFrom = &validFrom.String
		}
		if validTo.Valid {
			rp.DiscountValidTo = &validTo.String
		}
		if roomsLeft.Valid {
			v := int(roomsLeft.Int64)
			rp.RoomsLeft = &v
		}
		typeID := roomToType[rp.RoomID]
		plansByType[typeID] = append(plansByType[typeID], rp)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range types {
		types[i].RatePlans = append(types[i].RatePlans, plansByType[types[i].RoomTypeID]...)
	}
	return nil
}

func unmarshalStringArray(raw sql.NullString) ([]string, error) {
	if !raw.Valid || raw.String == "" {
		return []string{}, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw.String), &values); err != nil {
		return nil, err
	}
	return values, nil
}
