DROP INDEX uq_bus_seats_bus_seat_number ON bus_seats;
DROP INDEX uq_buses_service_number ON buses;

ALTER TABLE buses
  DROP COLUMN sale_open_at;
