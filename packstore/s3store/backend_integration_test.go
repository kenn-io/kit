package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestS3BackendConformance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	endpoint := os.Getenv("KIT_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("KIT_S3_TEST_ENDPOINT is not configured")
	}
	ctx := context.Background()
	prefix := "kit-conformance/" + pack.NewPackID()
	config := Config{
		Endpoint: endpoint, Region: "us-east-1",
		Bucket: envOr("KIT_S3_TEST_BUCKET", "kit-conformance"),
		Prefix: prefix, ForcePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			envOr("KIT_S3_TEST_ACCESS_KEY", "kit-test"),
			envOr("KIT_S3_TEST_SECRET_KEY", "kit-test-secret"),
			"",
		),
		InventoryPageSize: 1,
	}
	backend, err := New(ctx, config)
	require.NoError(err)
	ensureBucket(t, ctx, backend)
	t.Cleanup(func() { cleanPrefix(t, ctx, backend) })

	owner1 := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "vault-a", Store: "cold", Epoch: "epoch-1",
	}
	require.NoError(backend.ReplaceOwnership(ctx, owner1, nil))
	assert.Equal(owner1.Store, backend.StoreID())
	actual, err := backend.Ownership(ctx)
	require.NoError(err)
	assert.Equal(owner1, actual)

	report, err := backend.Probe(ctx)
	require.NoError(err)
	assert.Equal(CapabilityReport{
		StrongReadAfterWrite: true, RepeatableReads: true, RangeReads: true,
		MultipartPublication: true, Listing: true, Delete: true,
	}, report)

	content := bytes.Repeat([]byte("remote document\n"), 1024)
	hash := hashOf(content)
	loose, err := backend.PublishLoose(ctx, hash, bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: int64(len(content)),
		})
	require.NoError(err)
	assert.True(loose.Created)
	stream, size, err := backend.OpenLoose(ctx, hash, loose.Location)
	require.NoError(err)
	assert.Equal(int64(len(content)), size)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())
	assert.Equal(content, got)
	assert.True(stream.Verified())
	duplicate, err := backend.PublishLoose(ctx, hash, bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: int64(len(content)),
		})
	require.NoError(err)
	assert.False(duplicate.Created)
	_, err = backend.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(backend.bucket),
		Key: aws.String(backend.keys.loose(
			hash, packstore.LooseEncodingRaw,
		)),
		Body: bytes.NewReader([]byte("corrupt")),
	})
	require.NoError(err)
	repaired, err := backend.RepairLoose(
		ctx, hash, bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: int64(len(content)),
		},
	)
	require.NoError(err)
	assert.False(repaired.Created)
	stream, _, err = backend.OpenLoose(ctx, hash, repaired.Location)
	require.NoError(err)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())

	wrongHash := hashOf([]byte("different bytes"))
	_, err = backend.PublishLoose(ctx, wrongHash, bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: int64(len(content)),
		})
	require.ErrorIs(err, packstore.ErrContentMismatch)
	_, err = backend.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(backend.bucket),
		Key:    aws.String(backend.keys.loose(wrongHash, packstore.LooseEncodingRaw)),
	})
	assert.Equal(404, statusCode(err), "invalid source must remain an unpublished multipart orphan")

	packID, packBytes, indexed := makePack(t, content)
	packReceipt, err := backend.PublishPack(ctx, packID, bytes.NewReader(packBytes),
		packstore.PublishOptions{
			ExpectedSize: int64(len(packBytes)), SizeKnown: true,
			MaxBytes: int64(len(packBytes)),
		})
	require.NoError(err)
	assert.True(packReceipt.Created)
	packed, packedSize, err := backend.OpenPack(ctx, hash, indexed)
	require.NoError(err)
	assert.Equal(int64(len(content)), packedSize)
	got, err = io.ReadAll(packed)
	require.NoError(err)
	require.NoError(packed.Close())
	assert.Equal(content, got)
	assert.True(packed.Verified())

	_, err = backend.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(backend.bucket),
		Key:    aws.String(backend.keys.join("operator-note.txt")),
		Body:   bytes.NewReader([]byte("preserve me")),
	})
	require.NoError(err)
	objects, unknown := collectInventory(t, ctx, backend)
	assert.Len(objects, 2)
	assert.Contains(unknown, backend.keys.join("operator-note.txt"))

	oldBackend, err := New(ctx, Config{
		Endpoint: config.Endpoint, Region: config.Region, Bucket: config.Bucket,
		Prefix: config.Prefix, ForcePathStyle: true, Credentials: config.Credentials,
		ExpectedOwnership: &owner1,
	})
	require.NoError(err)
	owner2 := owner1
	owner2.Epoch = "epoch-2"
	require.NoError(backend.ReplaceOwnership(ctx, owner2, &owner1))
	err = oldBackend.Retire(ctx, packstore.ObjectRef{
		LooseHash: hash, LooseEncoding: packstore.LooseEncodingRaw,
	})
	require.ErrorIs(err, packstore.ErrStoreFenced)

	require.NoError(backend.Retire(ctx, packstore.ObjectRef{PackID: packID}))
	_, _, err = backend.OpenPack(ctx, hash, indexed)
	assert.ErrorIs(err, packstore.ErrPhysicalMissing)
}

func envOr(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func ensureBucket(t *testing.T, ctx context.Context, backend *Backend) {
	t.Helper()
	_, err := backend.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(backend.bucket),
	})
	if err != nil {
		_, headErr := backend.client.HeadBucket(ctx, &s3.HeadBucketInput{
			Bucket: aws.String(backend.bucket),
		})
		require.NoError(t, headErr, "create bucket: %v", err)
	}
}

func cleanPrefix(t *testing.T, ctx context.Context, backend *Backend) {
	t.Helper()
	var cursor *string
	for {
		output, err := backend.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(backend.bucket),
			Prefix:            aws.String(backend.keys.join("")),
			ContinuationToken: cursor,
		})
		require.NoError(t, err)
		for _, object := range output.Contents {
			_, err := backend.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(backend.bucket), Key: object.Key,
			})
			require.NoError(t, err)
		}
		if output.IsTruncated == nil || !*output.IsTruncated {
			break
		}
		cursor = output.NextContinuationToken
	}
}

func collectInventory(
	t *testing.T,
	ctx context.Context,
	backend *Backend,
) ([]packstore.InventoryObject, []string) {
	t.Helper()
	var objects []packstore.InventoryObject
	var unknown []string
	var cursor packstore.InventoryCursor
	for {
		page, err := backend.Inventory(ctx, cursor)
		require.NoError(t, err)
		objects = append(objects, page.Objects...)
		unknown = append(unknown, page.Unknown...)
		if page.NextCursor == "" {
			return objects, unknown
		}
		cursor = page.NextCursor
	}
}

func makePack(
	t *testing.T,
	content []byte,
) (string, []byte, packstore.IndexEntry) {
	t.Helper()
	dir := t.TempDir()
	writer, err := pack.NewWriter(dir, pack.WriterOptions{})
	require.NoError(t, err)
	entry, err := writer.Append(content)
	require.NoError(t, err)
	packID := pack.NewPackID()
	path := filepath.Join(dir, packID+".pack")
	_, err = writer.Seal(path)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	hash := hashOf(content)
	return packID, data, packstore.IndexEntry{
		Hash: hash, PackID: packID,
		Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen),
		RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
	}
}

func hashOf(content []byte) packstore.Hash {
	sum := sha256.Sum256(content)
	return packstore.Hash(hex.EncodeToString(sum[:]))
}
