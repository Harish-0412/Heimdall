package render

import "github.com/heimdall-dev/heimdall/internal/config"

// catalogImage is one of Heimdall's own images (not the application's),
// pinned by the digest of its multi-architecture index. The tag is for humans;
// the digest decides what runs.
//
// Images come from public mirrors without anonymous pull limits that bite
// clusters (ECR Public mirrors Docker Official Images with identical digests;
// quay.io hosts curl's official image). Platform.ImageMirror can redirect them
// to a pull-through cache; the digest still pins the content.
//
// To update a pin: `docker buildx imagetools inspect <ref>` and copy the
// index digest. Keep config's supported versions in step (TestCatalog).
type catalogImage struct {
	registry string // default registry and path prefix
	name     string // repository name under the registry, as on Docker Hub
	tag      string
	digest   string
	uid, gid int64 // the image's unprivileged user
}

func (i catalogImage) ref(mirror string) string {
	registry := i.registry
	if mirror != "" {
		registry = mirror
	}
	return registry + "/" + i.name + ":" + i.tag + "@" + i.digest
}

const ecrPublicDockerHub = "public.ecr.aws/docker"

// Alpine variants: smaller pulls mean faster environment start-up.
var postgresImages = map[string]catalogImage{
	"14": {ecrPublicDockerHub, "library/postgres", "14.24-alpine", "sha256:4ea9e5ed06591da7ea23eb65465e8d3187fe79f4d5ec3ae976d29a33b013e77a", 70, 70},
	"15": {ecrPublicDockerHub, "library/postgres", "15.19-alpine", "sha256:f7d23353e1b15400d22ebe31189f4d314b87a4c129cc400c8c2d8d4ca127bf81", 70, 70},
	"16": {ecrPublicDockerHub, "library/postgres", "16.15-alpine", "sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea", 70, 70},
	"17": {ecrPublicDockerHub, "library/postgres", "17.11-alpine", "sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24", 70, 70},
}

// Redis 7.2 is the last BSD-licensed line; later versions changed licence.
var redisImages = map[string]catalogImage{
	"6": {ecrPublicDockerHub, "library/redis", "6.2.24-alpine", "sha256:b362b5dff9d14d961dc3352db4776aba6a8c53ca2661d7c74ad2c121f82cdaea", 999, 1000},
	"7": {ecrPublicDockerHub, "library/redis", "7.2.16-alpine", "sha256:29e8589c3f9ba699b5f7aa4b3c7733c58852a3626439e619aa0ee78de08c6ca0", 999, 1000},
}

var rabbitMQImages = map[string]catalogImage{
	"3.12": {ecrPublicDockerHub, "library/rabbitmq", "3.12.14-alpine", "sha256:97907aceae0a6fbfb24847724adadf12f536a2b9bf547a42bc9dad153ed74e99", 100, 101},
	"3.13": {ecrPublicDockerHub, "library/rabbitmq", "3.13.7-alpine", "sha256:d7af1c87c5f1eda13fcfca06db452bf3aeab6619fc3358b68535c0c02c4e52bc", 100, 101},
}

// toolboxImage runs smoke tests: a shell and curl, nothing else.
var toolboxImage = catalogImage{"quay.io", "curl/curl", "8.22.0", "sha256:58adaa4e8dca9c988bae2aba4ab3434a0bb2da16bbe3f92dec39ec7785166777", 100, 101}

// dependencyImages returns the catalog for a managed dependency.
func dependencyImages(dep string) map[string]catalogImage {
	switch dep {
	case config.DepPostgres:
		return postgresImages
	case config.DepRedis:
		return redisImages
	case config.DepRabbitMQ:
		return rabbitMQImages
	}
	return nil
}
