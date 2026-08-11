USE tickets;

START TRANSACTION;

INSERT IGNORE INTO cities (name) VALUES
('Shanghai'),
('Hangzhou'),
('Suzhou'),
('Nanjing'),
('Ningbo');

SET @city_shanghai = (SELECT id FROM cities WHERE name = 'Shanghai');
SET @city_hangzhou = (SELECT id FROM cities WHERE name = 'Hangzhou');
SET @city_suzhou = (SELECT id FROM cities WHERE name = 'Suzhou');
SET @city_nanjing = (SELECT id FROM cities WHERE name = 'Nanjing');
SET @city_ningbo = (SELECT id FROM cities WHERE name = 'Ningbo');

INSERT IGNORE INTO terminals (city_id, name) VALUES
(@city_shanghai, 'Shanghai South'),
(@city_shanghai, 'Shanghai Hongqiao'),
(@city_hangzhou, 'Hangzhou East'),
(@city_hangzhou, 'Hangzhou West'),
(@city_suzhou, 'Suzhou North'),
(@city_nanjing, 'Nanjing South'),
(@city_ningbo, 'Ningbo South');

SET @terminal_shanghai_south = (
    SELECT id FROM terminals
    WHERE city_id = @city_shanghai AND name = 'Shanghai South'
);
SET @terminal_shanghai_hongqiao = (
    SELECT id FROM terminals
    WHERE city_id = @city_shanghai AND name = 'Shanghai Hongqiao'
);
SET @terminal_hangzhou_east = (
    SELECT id FROM terminals
    WHERE city_id = @city_hangzhou AND name = 'Hangzhou East'
);
SET @terminal_hangzhou_west = (
    SELECT id FROM terminals
    WHERE city_id = @city_hangzhou AND name = 'Hangzhou West'
);
SET @terminal_suzhou_north = (
    SELECT id FROM terminals
    WHERE city_id = @city_suzhou AND name = 'Suzhou North'
);
SET @terminal_nanjing_south = (
    SELECT id FROM terminals
    WHERE city_id = @city_nanjing AND name = 'Nanjing South'
);
SET @terminal_ningbo_south = (
    SELECT id FROM terminals
    WHERE city_id = @city_ningbo AND name = 'Ningbo South'
);

INSERT IGNORE INTO routes (origin_terminal_id, destination_terminal_id, duration, distance) VALUES
(@terminal_shanghai_south, @terminal_hangzhou_east, 120, 180),
(@terminal_hangzhou_east, @terminal_shanghai_south, 120, 180),
(@terminal_shanghai_hongqiao, @terminal_suzhou_north, 95, 110),
(@terminal_suzhou_north, @terminal_nanjing_south, 150, 215),
(@terminal_nanjing_south, @terminal_shanghai_hongqiao, 210, 300),
(@terminal_shanghai_south, @terminal_ningbo_south, 180, 230),
(@terminal_ningbo_south, @terminal_hangzhou_west, 140, 165),
(@terminal_hangzhou_west, @terminal_shanghai_hongqiao, 130, 175);

SET @route_shs_hze = (
    SELECT id FROM routes
    WHERE origin_terminal_id = @terminal_shanghai_south
      AND destination_terminal_id = @terminal_hangzhou_east
);
SET @route_hze_shs = (
    SELECT id FROM routes
    WHERE origin_terminal_id = @terminal_hangzhou_east
      AND destination_terminal_id = @terminal_shanghai_south
);
SET @route_shh_szn = (
    SELECT id FROM routes
    WHERE origin_terminal_id = @terminal_shanghai_hongqiao
      AND destination_terminal_id = @terminal_suzhou_north
);
SET @route_szn_njs = (
    SELECT id FROM routes
    WHERE origin_terminal_id = @terminal_suzhou_north
      AND destination_terminal_id = @terminal_nanjing_south
);
SET @route_njs_shh = (
    SELECT id FROM routes
    WHERE origin_terminal_id = @terminal_nanjing_south
      AND destination_terminal_id = @terminal_shanghai_hongqiao
);
SET @route_shs_nbs = (
    SELECT id FROM routes
    WHERE origin_terminal_id = @terminal_shanghai_south
      AND destination_terminal_id = @terminal_ningbo_south
);
SET @route_nbs_hzw = (
    SELECT id FROM routes
    WHERE origin_terminal_id = @terminal_ningbo_south
      AND destination_terminal_id = @terminal_hangzhou_west
);
SET @route_hzw_shh = (
    SELECT id FROM routes
    WHERE origin_terminal_id = @terminal_hangzhou_west
      AND destination_terminal_id = @terminal_shanghai_hongqiao
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_shs_hze,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 1 DAY), '08:00:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 1 DAY), '10:00:00'),
    40,
    88,
    'standard',
    'Transit Express',
    NULL,
    'TM-SH-HZ-0800',
    0
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-SH-HZ-0800'
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_shs_hze,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 1 DAY), '15:00:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 1 DAY), '17:00:00'),
    40,
    92,
    'standard',
    'Transit Express',
    NULL,
    'TM-SH-HZ-1500',
    0
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-SH-HZ-1500'
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_hze_shs,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 1 DAY), '09:30:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 1 DAY), '11:30:00'),
    38,
    86,
    'standard',
    'Transit Express',
    NULL,
    'TM-HZ-SH-0930',
    0
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-HZ-SH-0930'
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_shh_szn,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 1 DAY), '07:40:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 1 DAY), '09:15:00'),
    32,
    58,
    'standard',
    'Delta Coach',
    NULL,
    'TM-SH-SZ-0740',
    0
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-SH-SZ-0740'
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_szn_njs,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 2 DAY), '13:00:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 2 DAY), '15:30:00'),
    36,
    76,
    'standard',
    'Delta Coach',
    NULL,
    'TM-SZ-NJ-1300',
    0
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-SZ-NJ-1300'
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_njs_shh,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 2 DAY), '16:20:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 2 DAY), '19:50:00'),
    42,
    118,
    'vip',
    'Metro Star',
    NULL,
    'TM-NJ-SH-1620',
    1
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-NJ-SH-1620'
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_shs_nbs,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 3 DAY), '07:30:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 3 DAY), '10:30:00'),
    34,
    98,
    'standard',
    'Coastal Line',
    NULL,
    'TM-SH-NB-0730',
    0
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-SH-NB-0730'
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_nbs_hzw,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 3 DAY), '11:20:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 3 DAY), '13:40:00'),
    34,
    72,
    'standard',
    'Coastal Line',
    NULL,
    'TM-NB-HZ-1120',
    0
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-NB-HZ-1120'
);

INSERT INTO buses (
    route_id,
    departure_time,
    arrival_time,
    capacity,
    price,
    bus_type,
    corporation,
    super_corporation,
    service_number,
    is_vip
)
SELECT
    @route_hzw_shh,
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 3 DAY), '18:10:00'),
    TIMESTAMP(DATE_ADD(CURDATE(), INTERVAL 3 DAY), '20:20:00'),
    38,
    84,
    'standard',
    'Metro Star',
    NULL,
    'TM-HZ-SH-1810',
    0
FROM DUAL
WHERE NOT EXISTS (
    SELECT 1 FROM buses WHERE service_number = 'TM-HZ-SH-1810'
);

INSERT INTO bus_seats (bus_id, seat_number, status)
WITH RECURSIVE seat_numbers AS (
    SELECT 1 AS seat_number
    UNION ALL
    SELECT seat_number + 1
    FROM seat_numbers
    WHERE seat_number < 16
)
SELECT
    b.id,
    seat_numbers.seat_number,
    'available'
FROM buses b
JOIN seat_numbers
LEFT JOIN bus_seats bs
    ON bs.bus_id = b.id
   AND bs.seat_number = seat_numbers.seat_number
WHERE b.service_number IN (
    'TM-SH-HZ-0800',
    'TM-SH-HZ-1500',
    'TM-HZ-SH-0930',
    'TM-SH-SZ-0740',
    'TM-SZ-NJ-1300',
    'TM-NJ-SH-1620',
    'TM-SH-NB-0730',
    'TM-NB-HZ-1120',
    'TM-HZ-SH-1810'
)
AND bs.id IS NULL;

COMMIT;

SELECT id, name
FROM cities
ORDER BY id;

SELECT t.id, c.name AS city_name, t.name AS terminal_name
FROM terminals t
JOIN cities c ON c.id = t.city_id
ORDER BY c.name, t.name;

SELECT
    r.id,
    t1.name AS origin_terminal,
    t2.name AS destination_terminal,
    r.duration,
    r.distance
FROM routes r
JOIN terminals t1 ON t1.id = r.origin_terminal_id
JOIN terminals t2 ON t2.id = r.destination_terminal_id
ORDER BY r.id;

SELECT
    b.id,
    b.service_number,
    b.departure_time,
    b.arrival_time,
    b.price,
    b.capacity,
    COUNT(bs.id) AS seats_seeded
FROM buses b
LEFT JOIN bus_seats bs ON bs.bus_id = b.id
GROUP BY b.id, b.service_number, b.departure_time, b.arrival_time, b.price, b.capacity
ORDER BY b.departure_time, b.id;
