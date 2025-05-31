package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30 //1GB (setting an upload limit of 1GB)
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

	// 1. Extracting Video ID from URL and Parse it into UUID
	videoIdString := r.PathValue("videoID")
	videoId, err := uuid.Parse(videoIdString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}
	// 2. Validating the User to get the Owner of the Video
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}
	// 3. Getting the video details from the DB
	video, err := cfg.db.GetVideo(videoId)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not extract video by ID", err)
		return
	}
	// 3.1 Checking if the video Owneer in the DB is the same as the User obtained from JWT
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not the owner of this video", err)
		return
	}
	// 4. Parse the Video File from the Uploaded form Data (from the UI)
	file, handler, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()
	// 5. Getting the Media Type of the Resource and Validating it for mp4 type.
	mediaType, _, err := mime.ParseMediaType(handler.Header.Get("Content-Type"))
	assetPath := getAssetPath(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "could not parse the media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type, only MP4 is allowed", nil)
		return
	}

	// 6. Save the Uploaded File to a Temporary File on Disk(locally)
	os_file, err := os.CreateTemp("", "tubely-upload.mp4") // creating a temp file <tubely-upload.mp4> on current directory
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not create the temp file locally", err)
		return
	}
	defer os.Remove(os_file.Name())
	defer os_file.Close() //defer is LIFO, so it will close before the remove

	// 6. copy the content to the temp file <os_file>
	if _, err := io.Copy(os_file, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not write file to disk", err)
		return
	}

	_, err = os_file.Seek(0, io.SeekStart) // put the pointer back to front again
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not reset file pointer", err)
		return
	}

	// 7. Generate a s3 URL and put the <os_file> into S3 bucket

	url := cfg.getAssetURL(assetPath)
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(url),
		Body:        os_file,
		ContentType: aws.String(mediaType),
	})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading file to S3", err)
		return
	}

	s3VideoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, url)

	// 8. Update the URL to s3 bucket location and update all details in DB.
	video.VideoURL = &s3VideoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
}
