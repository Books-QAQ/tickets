CREATE TABLE orders (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    order_no VARCHAR(64) NOT NULL UNIQUE,
    user_id INT NOT NULL,
    bus_id INT NOT NULL,
    seat_id INT NOT NULL,
    amount INT NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'paid', 'canceled', 'refunded')),
    pay_channel VARCHAR(20),
    paid_at DATETIME,
    expired_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (bus_id) REFERENCES buses(id) ON DELETE CASCADE,
    FOREIGN KEY (seat_id) REFERENCES bus_seats(id) ON DELETE CASCADE
);

CREATE INDEX idx_orders_user ON orders (user_id);
CREATE INDEX idx_orders_status_expired ON orders (status, expired_at);
