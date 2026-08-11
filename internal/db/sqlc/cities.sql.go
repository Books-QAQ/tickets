package db

import "context"

const createCity = `-- name: CreateCity :one
INSERT INTO cities (name)
VALUES (?)
`

func (q *Queries) CreateCity(ctx context.Context, name string) (City, error) {
	result, err := q.db.ExecContext(ctx, createCity, name)
	if err != nil {
		return City{}, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return City{}, err
	}

	return q.GetCityByID(ctx, int32(id))
}

const getAllCities = `-- name: GetAllCities :many
SELECT id, name
FROM cities
`

func (q *Queries) GetAllCities(ctx context.Context) ([]City, error) {
	rows, err := q.db.QueryContext(ctx, getAllCities)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := []City{}
	for rows.Next() {
		var i City
		if err := rows.Scan(&i.ID, &i.Name); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getCityByID = `-- name: GetCityByID :one
SELECT id, name
FROM cities
WHERE id = ?
`

func (q *Queries) GetCityByID(ctx context.Context, id int32) (City, error) {
	row := q.db.QueryRowContext(ctx, getCityByID, id)
	var i City
	err := row.Scan(&i.ID, &i.Name)
	return i, err
}
