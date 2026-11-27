-- Migration 0001: the standalone chatbot's read-only hotel catalogue.
--
-- For the chatbot's OWN database only (ai_chatbot_test / ai_chatbot_dev /
-- ai_chatbot_prod). Never run against the MaynStay booking database:
-- cmd/dbtool refuses any database not named ai_chatbot_* or that contains
-- booking-system tables.
--
-- Column names follow MaynStay's catalogue (hotels, room_types, hotel_rooms,
-- room_rate_plans, landmarks) so the transferred repository SQL is
-- unchanged. There are no users, bookings, payments or vendor tables:
-- room_holds stands in for bookings in the availability count only.

CREATE TABLE IF NOT EXISTS `hotels` (
  `hotel_id`           BIGINT        NOT NULL AUTO_INCREMENT,
  `name`               VARCHAR(200)  NOT NULL,
  `location`           VARCHAR(200)  NOT NULL DEFAULT '',
  `description`        TEXT          NOT NULL,
  `city`               VARCHAR(100)  NOT NULL,
  `state`              VARCHAR(100)  NULL,
  `country`            VARCHAR(100)  NULL,
  `address`            VARCHAR(300)  NULL,
  `latitude`           DECIMAL(9,6)  NULL,
  `longitude`          DECIMAL(9,6)  NULL,
  `category`           VARCHAR(60)   NULL,
  -- Fallback "from" price for a hotel with no room types yet.
  `price_per_night`    DECIMAL(10,2) NOT NULL DEFAULT 0,
  `rating`             DECIMAL(3,2)  NOT NULL DEFAULT 0,
  `total_reviews`      INT           NOT NULL DEFAULT 0,
  `amenities`          JSON          NULL,
  `thumbnail_url`      VARCHAR(500)  NULL,
  `images`             JSON          NULL,
  -- Kept for JSON compatibility with the frontend; always 0 here.
  `owner_id`           BIGINT        NOT NULL DEFAULT 0,
  `is_active`          TINYINT(1)    NOT NULL DEFAULT 1,
  `is_deleted`         TINYINT(1)    NOT NULL DEFAULT 0,
  `free_cancellation`  TINYINT(1)    NOT NULL DEFAULT 0,
  `pay_at_property`    TINYINT(1)    NOT NULL DEFAULT 0,
  `instant_booking`    TINYINT(1)    NOT NULL DEFAULT 0,
  `cancellation_hours` INT           NULL,
  `child_policy`       VARCHAR(500)  NULL,
  `pet_policy`         VARCHAR(500)  NULL,
  `id_rules`           VARCHAR(500)  NULL,
  `gstin`              VARCHAR(15)   NULL,
  -- In MaynStay these came from vendor_settings.
  `check_in_time`      VARCHAR(5)    NULL,
  `check_out_time`     VARCHAR(5)    NULL,
  `tax_rate`           DECIMAL(5,2)  NOT NULL DEFAULT 0,
  `created_at`         DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`         DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`hotel_id`),
  INDEX `idx_hotels_city` (`city`),
  INDEX `idx_hotels_active` (`is_active`, `is_deleted`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `room_types` (
  `room_type_id`    BIGINT        NOT NULL AUTO_INCREMENT,
  `hotel_id`        BIGINT        NOT NULL,
  `name`            VARCHAR(120)  NOT NULL,
  `capacity`        INT           NOT NULL DEFAULT 2,
  `price_per_night` DECIMAL(10,2) NOT NULL,
  `description`     TEXT          NULL,
  `amenities`       JSON          NULL,
  `images`          JSON          NULL,
  `bed_type`        VARCHAR(60)   NULL,
  `size_sqm`        INT           NULL,
  `view_type`       VARCHAR(60)   NULL,
  PRIMARY KEY (`room_type_id`),
  INDEX `idx_room_types_hotel` (`hotel_id`),
  CONSTRAINT `fk_room_types_hotel` FOREIGN KEY (`hotel_id`) REFERENCES `hotels`(`hotel_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Physical rooms: total_rooms and available_count are counted from these.
CREATE TABLE IF NOT EXISTS `hotel_rooms` (
  `room_id`      BIGINT       NOT NULL AUTO_INCREMENT,
  `hotel_id`     BIGINT       NOT NULL,
  `room_type_id` BIGINT       NOT NULL,
  `room_number`  VARCHAR(20)  NOT NULL,
  `status`       VARCHAR(20)  NOT NULL DEFAULT 'Available',
  PRIMARY KEY (`room_id`),
  UNIQUE KEY `uq_hotel_rooms_number` (`hotel_id`, `room_number`),
  INDEX `idx_hotel_rooms_type` (`room_type_id`),
  CONSTRAINT `fk_hotel_rooms_type` FOREIGN KEY (`room_type_id`) REFERENCES `room_types`(`room_type_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Rate plans are stored per physical room (as in MaynStay); the catalogue
-- reads one representative room's plans per room type.
CREATE TABLE IF NOT EXISTS `room_rate_plans` (
  `rate_plan_id`        BIGINT        NOT NULL AUTO_INCREMENT,
  `room_id`             BIGINT        NOT NULL,
  `name`                VARCHAR(120)  NOT NULL,
  `description`         VARCHAR(500)  NOT NULL DEFAULT '',
  `price_per_night`     DECIMAL(10,2) NOT NULL,
  `max_occupancy`       INT           NOT NULL DEFAULT 2,
  `free_cancellation`   TINYINT(1)    NOT NULL DEFAULT 0,
  `pay_at_property`     TINYINT(1)    NOT NULL DEFAULT 0,
  `breakfast_included`  TINYINT(1)    NOT NULL DEFAULT 0,
  `inclusions`          JSON          NULL,
  `is_active`           TINYINT(1)    NOT NULL DEFAULT 1,
  `discount_type`       VARCHAR(20)   NOT NULL DEFAULT 'none',
  `discount_value`      DECIMAL(10,2) NOT NULL DEFAULT 0,
  `discount_valid_from` DATE          NULL,
  `discount_valid_to`   DATE          NULL,
  `rooms_left`          INT           NULL,
  `created_at`          DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`          DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`rate_plan_id`),
  INDEX `idx_rate_plans_room` (`room_id`, `is_active`),
  CONSTRAINT `fk_rate_plans_room` FOREIGN KEY (`room_id`) REFERENCES `hotel_rooms`(`room_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Rooms taken for a stay, so availability can be tested. Not bookings:
-- no guest, payment or personal data.
CREATE TABLE IF NOT EXISTS `room_holds` (
  `hold_id`      BIGINT      NOT NULL AUTO_INCREMENT,
  `room_type_id` BIGINT      NOT NULL,
  `check_in`     DATE        NOT NULL,
  `check_out`    DATE        NOT NULL,
  `room_count`   INT         NOT NULL DEFAULT 1,
  `status`       VARCHAR(20) NOT NULL DEFAULT 'confirmed',
  `created_at`   DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`hold_id`),
  INDEX `idx_room_holds_type_dates` (`room_type_id`, `status`, `check_in`, `check_out`),
  CONSTRAINT `fk_room_holds_type` FOREIGN KEY (`room_type_id`) REFERENCES `room_types`(`room_type_id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS `landmarks` (
  `landmark_id` BIGINT       NOT NULL AUTO_INCREMENT,
  `city`        VARCHAR(100) NOT NULL,
  `name`        VARCHAR(200) NOT NULL,
  `category`    VARCHAR(60)  NULL,
  `latitude`    DECIMAL(9,6) NOT NULL,
  `longitude`   DECIMAL(9,6) NOT NULL,
  `is_active`   TINYINT(1)   NOT NULL DEFAULT 1,
  `created_at`  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`landmark_id`),
  INDEX `idx_landmarks_city` (`city`, `is_active`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
