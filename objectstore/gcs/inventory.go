package gcs

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kit/objectstore"
)

// ListPage preserves every entry under prefix, including foreign names. Its
// opaque token is only for continuation, never permission to remove objects.
func (s *Store) ListPage(ctx context.Context, prefix, token string, size int) (objectstore.Page, error) {
	if err := objectstore.ValidatePrefix(prefix); err != nil {
		return objectstore.Page{}, err
	}
	if size == 0 {
		size = 1000
	}
	if size < 1 || size > 1000 {
		return objectstore.Page{}, errors.New("gcs: page size must be between 1 and 1000")
	}
	u := s.objectURL("")
	q := u.Query()
	q.Set("prefix", s.prefix+prefix)
	q.Set("max-keys", strconv.Itoa(size))
	q.Set("encoding-type", "url")
	q.Set("list-type", "2")
	if token != "" {
		q.Set("continuation-token", token)
	}
	u.RawQuery = q.Encode()
	response, _, err := s.request(ctx, http.MethodGet, u, nil, 0, nil)
	if err != nil {
		return objectstore.Page{}, err
	}
	if err := responseError(response); err != nil {
		return objectstore.Page{}, err
	}
	var result struct {
		Contents []struct {
			Key          string
			Size         int64
			Generation   string
			LastModified time.Time
		}
		IsTruncated           bool
		NextContinuationToken string
		EncodingType          string
	}
	decodeErr := xml.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&result)
	if err := errors.Join(decodeErr, response.Body.Close()); err != nil {
		return objectstore.Page{}, err
	}
	if len(result.Contents) > size {
		return objectstore.Page{}, errors.New("gcs: inventory exceeds page size")
	}
	page := objectstore.Page{}
	for _, item := range result.Contents {
		if result.EncodingType == "url" {
			key, err := url.PathUnescape(item.Key)
			if err != nil {
				return objectstore.Page{}, errors.New("gcs: invalid inventory key encoding")
			}
			item.Key = key
		}
		if !strings.HasPrefix(item.Key, s.prefix+prefix) || item.Size < 0 {
			return objectstore.Page{}, errors.New("gcs: invalid inventory entry")
		}
		page.Objects = append(page.Objects, objectstore.Metadata{Key: strings.TrimPrefix(item.Key, s.prefix), Size: item.Size, Revision: item.Generation, LastModified: item.LastModified.UTC()})
	}
	if result.IsTruncated {
		page.NextToken = result.NextContinuationToken
		if page.NextToken == "" || page.NextToken == token {
			return objectstore.Page{}, errors.New("gcs: truncated inventory has no advancing token")
		}
	}
	return page, nil
}
