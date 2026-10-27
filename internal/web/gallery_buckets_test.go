package web

import (
	"time"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/app"
	objstore "github.com/codeblocktz/yacht/internal/bucket"
)

// bucketGalleryPages are a bucket's tabs in each state they can be in.
func bucketGalleryPages() []galleryPage {
	now := time.Now()
	a := app.App{
		ID: uuid.New(), OwnerID: "owner-local", Name: "media", Namespace: app.Namespace("owner-local", "media"),
		Image: app.BucketImage, Source: app.SourceBucket, Internal: true, CreatedAt: now.Add(-72 * time.Hour),
	}
	b := app.Bucket{
		Name: "media-x7k2p9", Endpoint: "https://s3.datahuru.com", Region: "datahuru",
		Bytes: 412 << 20, Objects: 1284, MeasuredAt: now.Add(-3 * time.Minute),
	}
	d := AppDetailData{App: a, Tab: "", Bucket: &b}
	full := BucketListingData{App: "media", Bucket: b, Read: true, Notice: "Uploaded 3 files.", Listing: objstore.Listing{
		Prefix:  "photos/2026/",
		Folders: []string{"photos/2026/october/", "photos/2026/thumbnails/"},
		Objects: []objstore.Object{
			{Key: "photos/2026/cover.jpg", Size: 2 << 20, Modified: now.Add(-2 * time.Minute)},
			{Key: "photos/2026/a-very-long-file-name-that-somebody-exported-from-their-camera-roll-IMG_20261027_101500.heic", Size: 3_400_000, Modified: now.Add(-26 * time.Hour)},
			{Key: "photos/2026/notes.txt", Size: 812, Modified: now.Add(-9 * 24 * time.Hour)},
		},
		Next: "token",
	}}
	return []galleryPage{{
		file: "states-bucket.html", path: "/apps/media",
		crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "media"}, {Label: "Files"}},
		page: stack(
			section("Add › Bucket", "in the canvas's Add", panelWrap(canvasAddBucket("shop"))),
			section("Tabs", "files, variables, settings", bucketTabs(d)),
			section("Files", "a folder with folders and files, after an upload", stack(bucketFiles(d), BucketListing(full))),
			section("Empty", "a new bucket", BucketListing(BucketListingData{App: "media", Bucket: b, Read: true})),
			section("Store down", "the listing could not be read",
				BucketListing(BucketListingData{App: "media", Bucket: b, Error: "The object store is not answering. Files are safe; try again in a minute."})),
			section("Settings", "its deletion alone", bucketSettings(AppDetailData{App: a, Tab: "settings"})),
		),
	}}
}
