package s3store

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.kenn.io/kit/packstore"
)

const (
	defaultPartBytes     = int64(8 << 20)
	defaultInventorySize = int32(1000)
)

// Config binds one S3-compatible namespace. The bucket must already exist.
// ExpectedOwnership enables ordinary reads and destructive operations; an
// unattached backend may only inspect or replace its ownership marker.
type Config struct {
	Endpoint          string
	Region            string
	Bucket            string
	Prefix            string
	Credentials       aws.CredentialsProvider
	ForcePathStyle    bool
	ExpectedOwnership *packstore.Ownership
	PartBytes         int64
	InventoryPageSize int32
	Limits            packstore.Limits
}

// CapabilityReport records the endpoint behavior required by this backend.
type CapabilityReport struct {
	StrongReadAfterWrite bool
	RepeatableReads      bool
	RangeReads           bool
	MultipartPublication bool
	Listing              bool
	Delete               bool
}

// Backend owns S3-compatible byte mechanics without granting catalog
// authority.
type Backend struct {
	client *s3.Client
	bucket string
	keys   keyspace
	part   int64
	page   int32
	limits packstore.Limits
	mu     sync.RWMutex
	owner  *packstore.Ownership
	etag   string
}

// New constructs a backend and validates its static configuration. Probe
// performs the mutating endpoint-capability checks separately.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	if ctx == nil {
		return nil, fmt.Errorf("s3store: nil context")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3store: bucket is required")
	}
	keys, err := newKeyspace(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.PartBytes == 0 {
		cfg.PartBytes = defaultPartBytes
	}
	if cfg.PartBytes < 5<<20 {
		return nil, fmt.Errorf("s3store: multipart part size must be at least 5 MiB")
	}
	if cfg.InventoryPageSize == 0 {
		cfg.InventoryPageSize = defaultInventorySize
	}
	if cfg.InventoryPageSize < 1 || cfg.InventoryPageSize > 1000 {
		return nil, fmt.Errorf("s3store: inventory page size must be between 1 and 1000")
	}
	if cfg.ExpectedOwnership != nil {
		if err := cfg.ExpectedOwnership.Validate(); err != nil {
			return nil, err
		}
	}
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.Credentials != nil {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(cfg.Credentials))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("s3store: load AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = cfg.ForcePathStyle
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	backend := &Backend{
		client: client, bucket: cfg.Bucket, keys: keys,
		part: cfg.PartBytes, page: cfg.InventoryPageSize, limits: cfg.Limits,
	}
	if cfg.ExpectedOwnership != nil {
		copy := *cfg.ExpectedOwnership
		backend.owner = &copy
	}
	return backend, nil
}

// StoreID returns the configured stable store identity, or empty while
// unattached.
func (b *Backend) StoreID() packstore.StoreID {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.owner == nil {
		return ""
	}
	return b.owner.Store
}

func (b *Backend) expectedOwnership() (*packstore.Ownership, string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.owner == nil {
		return nil, ""
	}
	copy := *b.owner
	return &copy, b.etag
}

func (b *Backend) setOwnership(owner packstore.Ownership, etag string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	copy := owner
	b.owner = &copy
	b.etag = etag
}
