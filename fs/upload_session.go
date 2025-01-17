package fs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	log "github.com/sirupsen/logrus"
)

// 10MB is the recommended upload size according to the graph API docs
const chunkSize uint64 = 10 * 1024 * 1024

// upload states
const (
	uploadNotStarted = iota
	uploadStarted
	uploadComplete
	uploadErrored
)

// UploadSession contains a snapshot of the file we're uploading. We have to
// take the snapshot or the file may have changed on disk during upload (which
// would break the upload). It is not recommended to directly deserialize into
// this structure from API responses in case Microsoft ever adds a size, data,
// or modTime field to the response.
type UploadSession struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	UploadURL          string    `json:"uploadUrl"`
	ExpirationDateTime time.Time `json:"expirationDateTime"`
	Size               uint64    `json:"size,omitempty"`
	Data               []byte    `json:"data,omitempty"`
	Checksum           string    `json:"checksum,omitempty"`
	ModTime            time.Time `json:"modTime,omitempty"`
	retries            int

	mutex sync.Mutex
	state int
	error // embedded error tracks errors that killed an upload
}

// MarshalJSON implements a custom JSON marshaler to avoid race conditions
func (u *UploadSession) MarshalJSON() ([]byte, error) {
	u.mutex.Lock()
	defer u.mutex.Unlock()

	type SerializeableUploadSession UploadSession
	return json.Marshal((*SerializeableUploadSession)(u))
}

// UploadSessionPost is the initial post used to create an upload session
type UploadSessionPost struct {
	Name             string `json:"name,omitempty"`
	ConflictBehavior string `json:"@microsoft.graph.conflictBehavior,omitempty"`
	FileSystemInfo   `json:"fileSystemInfo,omitempty"`
}

// FileSystemInfo carries the filesystem metadata like Mtime/Atime
type FileSystemInfo struct {
	CreatedDateTime      time.Time `json:"createdDateTime,omitempty"`
	LastAccessedDateTime time.Time `json:"lastAccessedDateTime,omitempty"`
	LastModifiedDateTime time.Time `json:"lastModifiedDateTime,omitempty"`
}

// isLargeSession returns whether or not this is a formal upload session that
// must be registered with the API (over 4MB, according to the documentation).
func (u *UploadSession) isLargeSession() bool {
	return u.Size > 4*1024*1024
}

func (u *UploadSession) getState() int {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	return u.state
}

// setState is just a helper method to set the UploadSession state and make error checking
// a little more straightforwards.
func (u *UploadSession) setState(state int, err error) error {
	u.mutex.Lock()
	u.state = state
	u.error = err
	u.mutex.Unlock()
	return err
}

// NewUploadSession wraps an upload of a file into an UploadSession struct
// responsible for performing uploads for a file.
func NewUploadSession(inode *Inode, auth *graph.Auth) (*UploadSession, error) {
	id, err := inode.RemoteID(auth)
	if err != nil || isLocalID(id) {
		log.WithFields(log.Fields{
			"id":   id,
			"name": inode.Name(),
			"err":  err,
		}).Errorf("Could not obtain remote ID for upload.")
		return nil, err
	}

	inode.mutex.RLock()
	defer inode.mutex.RUnlock()

	// create a generic session for all files
	session := UploadSession{
		ID:      inode.DriveItem.ID,
		Name:    inode.DriveItem.Name,
		Size:    inode.DriveItem.Size,
		Data:    make([]byte, inode.DriveItem.Size),
		ModTime: *inode.DriveItem.ModTime,
	}
	if inode.data == nil {
		log.WithFields(log.Fields{
			"id":   inode.DriveItem.ID,
			"name": inode.DriveItem.Name,
		}).Error("Tried to dereference a nil pointer.")
		return nil, errors.New("inode data was nil")
	}
	copy(session.Data, *inode.data)

	if inode.DriveItem.File.Hashes.SHA1Hash != "" {
		session.Checksum = inode.DriveItem.File.Hashes.SHA1Hash
	} else if inode.DriveItem.File.Hashes.QuickXorHash != "" {
		session.Checksum = inode.DriveItem.File.Hashes.QuickXorHash
	} else {
		log.WithFields(log.Fields{
			"id":   inode.DriveItem.ID,
			"name": inode.DriveItem.Name,
		}).Error("both inode checksums were nil!")
		return nil, errors.New("both inode checksums were nil")
	}
	return &session, nil
}

// cancel the upload session by deleting the temp file at the endpoint.
func (u *UploadSession) cancel(auth *graph.Auth) {
	// is it an actual API upload session?
	if u.isLargeSession() {
		state := u.getState()
		if state == uploadStarted || state == uploadErrored {
			// dont care about result, this is purely us being polite to the server
			go graph.Delete(u.UploadURL, auth)
		}
	}
}

// Internal method used for uploading individual chunks of a DriveItem. We have
// to make things this way because the internal Put func doesn't work all that
// well when we need to add custom headers. Will return without an error if
// irrespective of HTTP status (errors are reserved for stuff that prevented
// the HTTP request at all).
func (u *UploadSession) uploadChunk(auth *graph.Auth, offset uint64) ([]byte, int, error) {
	if u.UploadURL == "" {
		return nil, -1, errors.New("UploadSession UploadURL cannot be empty")
	}

	// how much of the file are we going to upload?
	end := offset + chunkSize
	var reqChunkSize uint64
	if end > u.Size {
		end = u.Size
		reqChunkSize = end - offset + 1
	}
	if offset > u.Size {
		return nil, -1, errors.New("offset cannot be larger than DriveItem size")
	}

	auth.Refresh()

	client := &http.Client{}
	request, _ := http.NewRequest(
		"PUT",
		u.UploadURL,
		bytes.NewReader((u.Data)[offset:end]),
	)
	// no Authorization header - it will throw a 401 if present
	request.Header.Add("Content-Length", strconv.Itoa(int(reqChunkSize)))
	frags := fmt.Sprintf("bytes %d-%d/%d", offset, end-1, u.Size)
	log.WithField("id", u.ID).Info("Uploading ", frags)
	request.Header.Add("Content-Range", frags)

	resp, err := client.Do(request)
	if err != nil {
		// this is a serious error, not simply one with a non-200 return code
		return nil, -1, err
	}
	defer resp.Body.Close()
	response, _ := ioutil.ReadAll(resp.Body)
	return response, resp.StatusCode, nil
}

// verifyRemoteChecksum confirms that the newly-uploaded remote file matches the
// local checksum. Returns false if there is a mismatch.
func (u *UploadSession) verifyRemoteChecksum(response []byte) error {
	remote := graph.DriveItem{}
	if err := json.Unmarshal(response, &remote); err != nil {
		return u.setState(uploadErrored, err)
	}
	if !remote.VerifyChecksum(u.Checksum) {
		return u.setState(uploadErrored, errors.New("remote checksum did not match"))
	}
	return u.setState(uploadComplete, nil)
}

// Upload copies the file's contents to the server. Should only be called as a
// goroutine, or it can potentially block for a very long time. The uploadSession.error
// field contains errors to be handled if called as a goroutine.
func (u *UploadSession) Upload(auth *graph.Auth) error {
	log.WithField("id", u.ID).Debug("Uploading file.")
	u.setState(uploadStarted, nil)
	if !u.isLargeSession() {
		// small files handled in this block
		remote, err := graph.Put(
			fmt.Sprintf("/me/drive/items/%s/content", u.ID),
			auth,
			bytes.NewReader(u.Data),
		)
		if err != nil && strings.Contains(err.Error(), "resourceModified") {
			// retry the request after a second, likely the server is having issues
			time.Sleep(time.Second)
			remote, err = graph.Put(
				fmt.Sprintf("/me/drive/items/%s/content", u.ID),
				auth,
				bytes.NewReader(u.Data),
			)
		}
		if err != nil {
			return u.setState(uploadErrored, err)
		}
		return u.verifyRemoteChecksum(remote)
	}

	// must create a formal upload session with the API for large sessions
	sessionPostData, _ := json.Marshal(UploadSessionPost{
		ConflictBehavior: "replace",
		FileSystemInfo: FileSystemInfo{
			LastModifiedDateTime: u.ModTime,
		},
	})
	resp, err := graph.Post(
		fmt.Sprintf("/me/drive/items/%s/createUploadSession", u.ID),
		auth,
		bytes.NewReader(sessionPostData),
	)
	if err != nil {
		return u.setState(uploadErrored, err)
	}
	// populate UploadURL/expiration - we unmarshal into a fresh session here
	// just in case the API does something silly at a later date and overwrites
	// a field it shouldn't.
	tmp := UploadSession{}
	if err = json.Unmarshal(resp, &tmp); err != nil {
		return u.setState(uploadErrored, err)
	}
	u.UploadURL = tmp.UploadURL
	u.ExpirationDateTime = tmp.ExpirationDateTime

	// api upload session created successfully, now do actual content upload
	var status int
	nchunks := int(math.Ceil(float64(u.Size) / float64(chunkSize)))
	for i := 0; i < nchunks; i++ {
		resp, status, err = u.uploadChunk(auth, uint64(i)*chunkSize)
		if err != nil {
			log.WithFields(log.Fields{
				"id":      u.ID,
				"name":    u.Name,
				"chunk":   i,
				"nchunks": nchunks,
				"err":     err,
			}).Error("Error during chunk upload.")
			return u.setState(uploadErrored, err)
		}

		// retry server-side failures with an exponential back-off strategy. Will not
		// exit this loop unless it receives a non 5xx error or serious failure
		for backoff := 1; status >= 500; backoff *= 2 {
			log.WithFields(log.Fields{
				"id":      u.ID,
				"name":    u.Name,
				"chunk":   i,
				"nchunks": nchunks,
				"status":  status,
			}).Errorf("The OneDrive server is having issues, retrying chunk upload in %ds.", backoff)
			time.Sleep(time.Duration(backoff) * time.Second)
			resp, status, err = u.uploadChunk(auth, uint64(i)*chunkSize)
			if err != nil { // a serious, non 4xx/5xx error
				log.WithFields(log.Fields{
					"id":     u.ID,
					"name":   u.Name,
					"err":    err,
					"status": status,
				}).Error("Failed while retrying chunk upload after server-side error.")
				return u.setState(uploadErrored, err)
			}
		}

		// handle client-side errors
		if status >= 400 {
			return u.setState(uploadErrored, errors.New(string(resp)))
		}
	}
	return u.verifyRemoteChecksum(resp)
}
