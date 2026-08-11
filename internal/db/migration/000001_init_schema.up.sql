-- init_schema.up.sql

CREATE TABLE users (
    id INT AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(255) UNIQUE NOT NULL,
    hashed_password VARCHAR(255) NOT NULL,
    full_name VARCHAR(255) NOT NULL,
    password_changed_at DATETIME NOT NULL DEFAULT '1970-01-01 00:00:00',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE cities (
    id INT AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(255) UNIQUE NOT NULL
);

CREATE TABLE terminals (
    id INT AUTO_INCREMENT PRIMARY KEY,
    city_id INT NOT NULL,
    name VARCHAR(255) NOT NULL,
    UNIQUE(city_id, name),
    FOREIGN KEY (city_id) REFERENCES cities(id) ON DELETE CASCADE
);

CREATE TABLE routes (
    id INT AUTO_INCREMENT PRIMARY KEY,
    origin_terminal_id INT NOT NULL,
    destination_terminal_id INT NOT NULL,
    duration INT NOT NULL,
    distance INT NOT NULL CHECK (distance > 0),
    UNIQUE(origin_terminal_id, destination_terminal_id),
    FOREIGN KEY (origin_terminal_id) REFERENCES terminals(id) ON DELETE CASCADE,
    FOREIGN KEY (destination_terminal_id) REFERENCES terminals(id) ON DELETE CASCADE
);

CREATE TABLE buses (
    id INT AUTO_INCREMENT PRIMARY KEY,
    route_id INT NOT NULL,
    departure_time DATETIME NOT NULL,
    arrival_time DATETIME NOT NULL,
    capacity INT NOT NULL CHECK (capacity > 0),
    price INT NOT NULL CHECK (price > 0),
    bus_type VARCHAR(255) NOT NULL,
    corporation VARCHAR(255),
    super_corporation VARCHAR(255),
    service_number VARCHAR(255),
    is_vip BOOLEAN DEFAULT FALSE,
    FOREIGN KEY (route_id) REFERENCES routes(id) ON DELETE CASCADE
);

CREATE TABLE bus_seats (
    id INT AUTO_INCREMENT PRIMARY KEY,
    bus_id INT NOT NULL,
    seat_number INT NOT NULL CHECK (seat_number > 0),
    status VARCHAR(20) NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'reserved', 'purchased', 'maintenance', 'broken')),
    FOREIGN KEY (bus_id) REFERENCES buses(id) ON DELETE CASCADE
);

CREATE TABLE seat_reservations (
    id INT AUTO_INCREMENT PRIMARY KEY,
    bus_id INT NOT NULL,
    bus_seat_id INT NOT NULL,
    user_id INT NOT NULL,
    status VARCHAR(20) NOT NULL CHECK (status IN ('reserved', 'purchased', 'canceled')),
    reserved_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    purchased_at DATETIME,
    FOREIGN KEY (bus_id) REFERENCES buses(id) ON DELETE CASCADE,
    FOREIGN KEY (bus_seat_id) REFERENCES bus_seats(id) ON DELETE CASCADE,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE tickets (
    id INT AUTO_INCREMENT PRIMARY KEY,
    user_id INT NOT NULL,
    bus_id INT NOT NULL,
    seat_reservation_id INT NOT NULL,
    status VARCHAR(20) NOT NULL CHECK (status IN ('reserved', 'purchased', 'canceled')),
    reserved_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    purchased_at DATETIME,
    UNIQUE(user_id, bus_id, seat_reservation_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (bus_id) REFERENCES buses(id) ON DELETE CASCADE,
    FOREIGN KEY (seat_reservation_id) REFERENCES seat_reservations(id) ON DELETE CASCADE
);

CREATE TABLE penalties (
    id INT AUTO_INCREMENT PRIMARY KEY,
    bus_id INT NOT NULL,
    actual_hours_before FLOAT CHECK (actual_hours_before >= 0),
    hours_before FLOAT CHECK (hours_before >= 0),
    percent INT NOT NULL CHECK (percent >= 0 AND percent <= 100),
    custom_text TEXT,
    FOREIGN KEY (bus_id) REFERENCES buses(id) ON DELETE CASCADE
);

CREATE TABLE sessions (
  id CHAR(36) PRIMARY KEY,
  username VARCHAR(255) NOT NULL,
  refresh_token TEXT NOT NULL,
  user_agent TEXT NOT NULL,
  client_ip TEXT NOT NULL,
  is_blocked BOOLEAN NOT NULL DEFAULT false,
  expires_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (username) REFERENCES users (username) ON DELETE CASCADE
);

CREATE INDEX idx_routes_origin_destination ON routes (origin_terminal_id, destination_terminal_id);
CREATE INDEX idx_buses_departure_time ON buses (departure_time);
-- CREATE INDEX idx_bus_seats_status ON bus_seats (status);
CREATE INDEX idx_tickets_reserved_at ON tickets (reserved_at);
CREATE INDEX idx_sessions_username ON sessions (username);
