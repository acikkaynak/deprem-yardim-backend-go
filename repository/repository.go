package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ggwhite/go-masker"
	"github.com/lib/pq"

	"github.com/acikkaynak/backend-api-go/needs"

	sq "github.com/Masterminds/squirrel"
	"github.com/acikkaynak/backend-api-go/feeds"
	pgx "github.com/jackc/pgx/v5"
)

var (
	psql = sq.StatementBuilder.PlaceholderFormat(sq.Dollar)
)

type PgxIface interface {
	Begin(context.Context) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	Close()
}

type Repository struct {
	pool PgxIface
}

func New(p PgxIface) *Repository {
	return &Repository{
		pool: p,
	}
}

func (repo *Repository) Close() {
	repo.pool.Close()
}

func (repo *Repository) GetLocations(swLat, swLng, neLat, neLng float64, timestamp int64, reason, channel string, extraParams bool, isLocationVerified, isNeedVerified string) ([]feeds.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	selectBuilder := psql.
		Select("id", "latitude", "longitude", "entry_id", "epoch", "reason", "channel", "is_location_verified", "is_need_verified").
		From("feeds_location")

	if extraParams == true {
		selectBuilder = selectBuilder.Column("extra_parameters")
	}

	if swLat != 0.0 || swLng != 0.0 || neLat != 0.0 || neLng != 0.0 {
		selectBuilder = selectBuilder.Where(sq.GtOrEq{"southwest_lat": swLat, "southwest_lng": swLng}).
			Where(sq.LtOrEq{"northeast_lat": neLat, "northeast_lng": neLng})
	}

	if timestamp != 0 {
		if channel != "ahbap_location" {
			selectBuilder = selectBuilder.Where("epoch >= ?", timestamp)
		}
	}

	if reason != "" {
		selectBuilder = selectBuilder.Where("reason ILIKE ANY(?)", pq.Array(strings.Split(reason, ",")))
	}

	if channel != "" {
		selectBuilder = selectBuilder.Where("channel ILIKE ANY(?)", pq.Array(strings.Split(channel, ",")))
	}

	if isLocationVerified != "" {
		selectBuilder = selectBuilder.Where(sq.Eq{"is_location_verified": isLocationVerified})
	}

	if isNeedVerified != "" {
		selectBuilder = selectBuilder.Where(sq.Eq{"is_need_verified": isNeedVerified})
	}

	newSql, args, err := selectBuilder.ToSql()
	if err != nil {
		return nil, fmt.Errorf("could not format query : %w", err)
	}

	query, err := repo.pool.Query(ctx, newSql, args...)
	if err != nil {
		return nil, fmt.Errorf("could not query locations: %w", err)
	}

	var results []feeds.Result

	for query.Next() {
		var result feeds.Result
		result.Loc = make([]float64, 2)

		if extraParams {
			err := query.Scan(&result.ID,
				&result.Loc[0],
				&result.Loc[1],
				&result.Entry_ID,
				&result.Epoch,
				&result.Reason,
				&result.Channel,
				&result.IsLocationVerified,
				&result.IsNeedVerified,
				&result.ExtraParameters)
			if err != nil {
				continue
				// return nil, fmt.Errorf("could not scan locations: %w", err)
			}

			if *result.Channel == "twitter" || *result.Channel == "discord" || *result.Channel == "babala" {
				result.ExtraParameters = maskFields(result.ExtraParameters)
			}
		} else {
			err := query.Scan(&result.ID,
				&result.Loc[0],
				&result.Loc[1],
				&result.Entry_ID,
				&result.Epoch,
				&result.Reason,
				&result.Channel,
				&result.IsLocationVerified,
				&result.IsNeedVerified)
			if err != nil {
				continue
				// return nil, fmt.Errorf("could not scan locations: %w", err)
			}
		}

		results = append(results, result)
	}

	return results, nil
}

func maskFields(extraParams *string) *string {
	if extraParams == nil || *extraParams == "" {
		return nil
	}

	var jsonMap map[string]interface{}
	extraParamsStr := strings.ReplaceAll(*extraParams, " nan,", "'',")
	extraParamsStr = strings.ReplaceAll(extraParamsStr, " nan}", "''}")
	extraParamsStr = strings.ReplaceAll(extraParamsStr, "\\", "")

	if err := json.Unmarshal([]byte(strings.ReplaceAll(extraParamsStr, "'", "\"")), &jsonMap); err != nil {
		return nil
	}

	jsonMap["tel"] = masker.Telephone(fmt.Sprintf("%v", jsonMap["tel"]))
	jsonMap["telefon"] = masker.Telephone(fmt.Sprintf("%v", jsonMap["telefon"]))
	jsonMap["numara"] = masker.Telephone(fmt.Sprintf("%v", jsonMap["numara"]))
	jsonMap["isim-soyisim"] = masker.Name(fmt.Sprintf("%v", jsonMap["isim-soyisim"]))
	jsonMap["name_surname"] = masker.Name(fmt.Sprintf("%v", jsonMap["name_surname"]))
	jsonMap["name"] = masker.Name(fmt.Sprintf("%v", jsonMap["name"]))
	marshal, _ := json.Marshal(jsonMap)
	s := string(marshal)
	return &s
}

func (repo *Repository) GetFeed(id int64) (*feeds.Feed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	row := repo.pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT fe.id, full_text, is_resolved, fe.channel, fe.timestamp, fe.extra_parameters, fl.formatted_address, fl.reason "+
			"FROM feeds_entry fe, feeds_location fl "+
			"WHERE fe.id = fl.entry_id AND fe.id=%d", id))

	var feed feeds.Feed
	if err := row.Scan(&feed.ID, &feed.FullText, &feed.IsResolved, &feed.Channel, &feed.Timestamp, &feed.ExtraParameters, &feed.FormattedAddress, &feed.Reason); err != nil {
		return nil, fmt.Errorf("could not query feed with id : %w", err)
	}

	if feed.Channel == "twitter" || feed.Channel == "discord" || feed.Channel == "babala" {
		feed.ExtraParameters = maskFields(feed.ExtraParameters)
	}

	return &feed, nil
}

func (repo *Repository) GetNeeds(onlyNotResolved bool) ([]needs.Need, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	q := "SELECT n.id, n.description, n.is_resolved, n.timestamp, n.extra_parameters, n.formatted_address, n.latitude, n.longitude " +
		"FROM needs n"
	if onlyNotResolved {
		q = fmt.Sprintf("%s WHERE n.is_resolved=false", q)
	}
	query, err := repo.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("could not query needs: %w", err)
	}

	var results []needs.Need
	for query.Next() {
		var result needs.Need
		result.Loc = make([]float64, 2)

		err := query.Scan(&result.ID,
			&result.Description,
			&result.IsResolved,
			&result.Timestamp,
			&result.ExtraParameters,
			&result.FormattedAddress,
			&result.Loc[0],
			&result.Loc[1])
		if err != nil {
			continue
		}

		results = append(results, result)
	}

	return results, nil
}

func (repo *Repository) CreateNeed(address, description string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	q := `INSERT INTO needs(address, description, timestamp, is_resolved, formatted_address, latitude, longitude) VALUES ($1::varchar, $2::varchar, $3::timestamp, $4::bool, $5::varchar, $6::int, $7::int) RETURNING id`

	var id int64
	err := repo.pool.QueryRow(ctx, q, address, description, time.Now(), false, "", 0, 0).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("could not query needs: %w", err)
	}

	return id, nil
}

func (repo *Repository) CreateFeed(ctx context.Context, feed feeds.Feed, location feeds.Location) (error, int64) {
	tx, err := repo.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("error transaction begin stage %w", err), 0
	}
	defer tx.Rollback(ctx)

	entryID, err := repo.createFeedEntry(ctx, tx, feed)
	if err != nil {
		return err, 0
	}

	location.EntryID = entryID
	if _, err = repo.createFeedLocation(ctx, tx, location); err != nil {
		return err, 0
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("error transaction commit stage %w", err), 0
	}

	return nil, entryID
}

func (repo *Repository) createFeedEntry(ctx context.Context, tx pgx.Tx, feed feeds.Feed) (int64, error) {
	q := `INSERT INTO feeds_entry (
				full_text, is_resolved, channel, 
				extra_parameters, "timestamp", epoch,
				is_geolocated, reason
			)
			values (
				$1, $2, $3,
				$4, $5, $6,
				$7, $8
			) RETURNING id;`

	var id int64
	err := tx.QueryRow(ctx, q,
		feed.FullText, feed.IsResolved, feed.Channel,
		feed.ExtraParameters, feed.Timestamp, feed.Epoch,
		false, feed.Reason).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("could not insert feeds entry: %w", err)
	}

	return id, nil
}

func (repo *Repository) createFeedLocation(ctx context.Context, tx pgx.Tx, location feeds.Location) (int64, error) {
	q := `INSERT INTO feeds_location (
			formatted_address, 
			latitude, longitude, 
			northeast_lat, northeast_lng, 
			southwest_lat, southwest_lng, 
			entry_id, "timestamp", 
			epoch, reason, channel
		) values (
			$1, $2, $3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12
		) RETURNING id;`

	var id int64

	if location.FormattedAddress != "" && location.Latitude != 0 && location.Longitude != 0 {
		err := tx.QueryRow(ctx, q,
			location.FormattedAddress,
			location.Latitude, location.Longitude,
			location.NortheastLat, location.NortheastLng,
			location.SouthwestLat, location.SouthwestLng,
			location.EntryID, location.Timestamp,
			location.Epoch, location.Reason, location.Channel,
		).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("could not insert feeds location: %w", err)
		}
	}

	return id, nil
}

func (repo *Repository) UpdateLocationIntent(ctx context.Context, id int64, intents string) error {
	q := "UPDATE feeds_location SET reason = $1 WHERE entry_id=$2;"

	_, err := repo.pool.Exec(ctx, q, intents, id)
	if err != nil {
		return fmt.Errorf("could not update feeds location intent: %w", err)
	}

	return nil
}

func (repo *Repository) UpdateFeedLocations(ctx context.Context, locations []feeds.FeedLocation) error {
	batch := &pgx.Batch{}
	for _, location := range locations {
		batch.Queue("UPDATE feeds_location SET is_verified = true, latitude = $1, longitude = $2, formatted_address = $3 WHERE entry_id = $4;", location.Latitude, location.Longitude, location.Address, location.EntryID)
	}
	_, err := repo.pool.SendBatch(ctx, batch).Exec()
	return err
}
