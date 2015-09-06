package fs

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Sirupsen/logrus"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"github.com/stretchr/testify/assert"
	"github.com/zach-klippenstein/goadb"
)

func TestAsFuseDirEntriesNoErr(t *testing.T) {
	entries := []*goadb.DirEntry{
		&goadb.DirEntry{
			Name: "/foo.txt",
			Size: 24,
			Mode: 0444,
		},
		&goadb.DirEntry{
			Name: "/bar.txt",
			Size: 42,
			Mode: 0444,
		},
	}

	fuseEntries := asFuseDirEntries(entries)
	assert.Len(t, fuseEntries, 2)
	assert.Equal(t, "/foo.txt", fuseEntries[0].Name)
	assert.NotEqual(t, 0, fuseEntries[0].Mode)
	assert.Equal(t, "/bar.txt", fuseEntries[1].Name)
	assert.NotEqual(t, 0, fuseEntries[1].Mode)
}

func TestSummarizeByteSlicesForLog(t *testing.T) {
	vals := []interface{}{
		"foo",
		[]byte("bar"),
		42,
	}

	summarizeByteSlicesForLog(vals)

	assert.Equal(t, "foo", vals[0])
	assert.Equal(t, []interface{}{
		"foo",
		"[]byte(3)",
		42,
	}, vals)
}

func TestLoggingFile(t *testing.T) {
	var logOut bytes.Buffer
	log := &logrus.Logger{
		Out:       &logOut,
		Formatter: new(logrus.JSONFormatter),
		Level:     logrus.DebugLevel,
	}
	flags := 42

	file := newLoggingFile(nodefs.NewDataFile([]byte{}), log)
	code := file.Fsync(flags)

	var output map[string]interface{}
	assert.NoError(t, json.Unmarshal(logOut.Bytes(), &output))

	assert.False(t, code.Ok())
	assert.Equal(t, "Fsync", output["operation"])
	assert.Equal(t, "[42]", output["args"])
	assert.NotEmpty(t, output["results"].(string))
	assert.NotEmpty(t, output["time"])
}
