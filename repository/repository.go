package repository

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/acikkaynak/backend-api-go/feeds"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	getLocationsQuery = "SELECT " +
		"id, " +
		"latitude, " +
		"longitude, " +
		"entry_id, " +
		"timestamp, " +
		"epoch " +
		"FROM feeds_location " +
		"where southwest_lat >= %f " +
		"and southwest_lng >= %f " +
		"and northeast_lat <= %f " +
		"and northeast_lng <= %f " +
		"and epoch >= %d"
)

type Repository struct {
	pool *pgxpool.Pool
}

func New() *Repository {
	dbUrl := os.Getenv("DB_CONN_STR")
	pool, err := pgxpool.New(context.Background(), dbUrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to connect to database: %v\n", err)
		os.Exit(1)
	}

	return &Repository{
		pool: pool,
	}
}

func (repo *Repository) Close() {
	repo.pool.Close()
}

func (repo *Repository) GetBusyLocation(data []feeds.Result, timestamp int64) (feeds.LatLng) {
  count = len(data)
  totalLat = 0.0
  totalLng = 0.0
  for _, result := range data {
    lat = result.Loc[0]
    lng = result.Loc[1]
    totalLat += lat
    totalLng += lng
  }
  avgLat = totalLat / count
  avgLng = totalLng / count
  
  var result feeds.LatLng
  result.Lat = avgLat
  result.Lng = avgLng

  return result
}

func (repo *Repository) GetLocations(swLat, swLng, neLat, neLng float64, timestamp int64) ([]feeds.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	query, err := repo.pool.Query(ctx, fmt.Sprintf(getLocationsQuery, swLat, swLng, neLat, neLng, timestamp))
	if err != nil {
		return nil, fmt.Errorf("could not query locations: %w", err)
	}

	var results []feeds.Result

	for query.Next() {
		var result feeds.Result
		result.Loc = make([]float64, 2)

		err := query.Scan(&result.ID,
			&result.Loc[0],
			&result.Loc[1],
			&result.Entry_ID,
			&result.Timestamp,
			&result.Epoch)
		if err != nil {
			continue
			//return nil, fmt.Errorf("could not scan locations: %w", err)
		}

		results = append(results, result)
	}

	return results, nil
}

func (repo *Repository) GetFeed(id int64) (*feeds.Feed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	row := repo.pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT fe.id, full_text, is_resolved, channel, fe.timestamp, fe.extra_parameters, fl.formatted_address "+
			"FROM feeds_entry fe, feeds_location fl "+
			"WHERE fe.id = fl.entry_id AND fe.id=%d", id))

	var feed feeds.Feed
	if err := row.Scan(&feed.ID, &feed.FullText, &feed.IsResolved, &feed.Channel, &feed.Timestamp, &feed.ExtraParameters, &feed.FormattedAddress); err != nil {
		return nil, fmt.Errorf("could not query feed with id : %w", err)
	}

	return &feed, nil
}
