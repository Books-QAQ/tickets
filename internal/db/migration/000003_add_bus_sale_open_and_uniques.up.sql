ALTER TABLE buses
  ADD COLUMN sale_open_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP AFTER arrival_time;

CREATE UNIQUE INDEX uq_buses_service_number ON buses (service_number);
CREATE UNIQUE INDEX uq_bus_seats_bus_seat_number ON bus_seats (bus_id, seat_number);
