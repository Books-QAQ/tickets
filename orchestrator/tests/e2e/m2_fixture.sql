-- M2 端到端夹具（可重复执行：先按标记清理，再插入）
-- 原则：不碰项目既有数据（只操作自己按 username/service_number 标记的行）

-- 时区：**必须**与本机/应用一致（应用 DSN 用 loc=Asia/Shanghai，而容器会话默认 UTC）
-- 不设这一行，夹具写进去的 NOW() 会差 8 小时 → "待支付订单"看起来已过期（M2 实测踩过）
SET time_zone = '+08:00';

SET FOREIGN_KEY_CHECKS = 0;
DELETE FROM orders WHERE user_id IN (SELECT id FROM users WHERE username IN ('cs_test_a','cs_test_b'));
DELETE FROM tickets WHERE user_id IN (SELECT id FROM users WHERE username IN ('cs_test_a','cs_test_b'));
DELETE FROM seat_reservations WHERE user_id IN (SELECT id FROM users WHERE username IN ('cs_test_a','cs_test_b'));
DELETE FROM users WHERE username IN ('cs_test_a','cs_test_b');
DELETE FROM penalties WHERE bus_id IN (SELECT id FROM buses WHERE service_number IN ('CSTEST1','CSTEST0'));
DELETE FROM bus_seats WHERE bus_id IN (SELECT id FROM buses WHERE service_number IN ('CSTEST1','CSTEST0'));
DELETE FROM buses WHERE service_number IN ('CSTEST1','CSTEST0');
SET FOREIGN_KEY_CHECKS = 1;

-- 两个用户：A 有车票/订单，B 没有（用于越权矩阵）
INSERT INTO users (username, hashed_password, full_name) VALUES
  ('cs_test_a', 'x-not-a-real-hash', '测试甲'),
  ('cs_test_b', 'x-not-a-real-hash', '测试乙');
SET @ua := (SELECT id FROM users WHERE username = 'cs_test_a');
SET @ub := (SELECT id FROM users WHERE username = 'cs_test_b');

-- 班次 1：3 天后发车，票价 120（与 Go 单测样例同价，便于人工核对 12.00 / 108.00）
INSERT INTO buses (route_id, departure_time, arrival_time, capacity, price, bus_type, service_number, is_vip)
VALUES (1, DATE_ADD(NOW(), INTERVAL 72 HOUR), DATE_ADD(NOW(), INTERVAL 76 HOUR), 10, 120, 'test', 'CSTEST1', 0);
SET @bus1 := LAST_INSERT_ID();

-- 阶梯规则：24h(含)以前 10%，2h(含)以前 30%，0.5h(含)以前 50%
-- actual_hours_before 留 NULL —— 正是 19.2 待确认的那一列
INSERT INTO penalties (bus_id, hours_before, actual_hours_before, percent, custom_text) VALUES
  (@bus1, 24,  NULL, 10, NULL),
  (@bus1, 2,   NULL, 30, NULL),
  (@bus1, 0.5, NULL, 50, NULL);

INSERT INTO bus_seats (bus_id, seat_number, status) VALUES (@bus1, 1, 'purchased'), (@bus1, 2, 'available');
SET @seat1 := (SELECT id FROM bus_seats WHERE bus_id = @bus1 AND seat_number = 1);

INSERT INTO seat_reservations (bus_id, bus_seat_id, user_id, status, reserved_at, purchased_at)
VALUES (@bus1, @seat1, @ua, 'purchased', NOW(), NOW());
SET @sr1 := LAST_INSERT_ID();

INSERT INTO tickets (user_id, bus_id, seat_reservation_id, status, reserved_at, purchased_at)
VALUES (@ua, @bus1, @sr1, 'purchased', NOW(), NOW());

-- 订单 A1：已支付（退票费计算 / 订单详情）
INSERT INTO orders (order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at)
VALUES ('11111111-1111-1111-1111-111111111111', @ua, @bus1, @seat1, 120, 'paid', 'mock', NOW(), DATE_ADD(NOW(), INTERVAL 1 DAY));

-- 订单 A2：待支付，剩余 15 分钟（待支付订单工具）
INSERT INTO orders (order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at)
VALUES ('22222222-2222-2222-2222-222222222222', @ua, @bus1, 2, 120, 'pending', NULL, NULL, DATE_ADD(NOW(), INTERVAL 15 MINUTE));

-- 订单 A3：已退款（退款进度工具）
INSERT INTO orders (order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at)
VALUES ('33333333-3333-3333-3333-333333333333', @ua, @bus1, 2, 98, 'refunded', 'alipay', NOW(), DATE_ADD(NOW(), INTERVAL 1 DAY));

-- 订单 B1：属于用户 B（越权测试：A 拿这个订单号去查，必须"未查询到"）
INSERT INTO orders (order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at)
VALUES ('44444444-4444-4444-4444-444444444444', @ub, @bus1, 2, 120, 'paid', 'mock', NOW(), DATE_ADD(NOW(), INTERVAL 1 DAY));

-- 班次 2：已过发车 3 小时（误车情形 → 不允许算手续费，转人工）
INSERT INTO buses (route_id, departure_time, arrival_time, capacity, price, bus_type, service_number, is_vip)
VALUES (1, DATE_SUB(NOW(), INTERVAL 3 HOUR), DATE_SUB(NOW(), INTERVAL 1 HOUR), 10, 120, 'test', 'CSTEST0', 0);
SET @bus0 := LAST_INSERT_ID();
INSERT INTO penalties (bus_id, hours_before, actual_hours_before, percent, custom_text) VALUES
  (@bus0, 24, NULL, 10, NULL);
INSERT INTO bus_seats (bus_id, seat_number, status) VALUES (@bus0, 1, 'purchased');
SET @seat0 := (SELECT id FROM bus_seats WHERE bus_id = @bus0 AND seat_number = 1);
INSERT INTO seat_reservations (bus_id, bus_seat_id, user_id, status, reserved_at, purchased_at)
VALUES (@bus0, @seat0, @ua, 'purchased', NOW(), NOW());
SET @sr0 := LAST_INSERT_ID();
INSERT INTO tickets (user_id, bus_id, seat_reservation_id, status, reserved_at, purchased_at)
VALUES (@ua, @bus0, @sr0, 'purchased', NOW(), NOW());
INSERT INTO orders (order_no, user_id, bus_id, seat_id, amount, status, pay_channel, paid_at, expired_at)
VALUES ('55555555-5555-5555-5555-555555555555', @ua, @bus0, @seat0, 120, 'paid', 'mock', NOW(), DATE_ADD(NOW(), INTERVAL 1 DAY));

SELECT @ua AS user_a, @ub AS user_b, @bus1 AS bus_future, @bus0 AS bus_departed;
