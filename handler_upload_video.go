package main

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	http.MaxBytesReader(w, r.Body, 1<<30)

	videoIdString := r.PathValue("videoID")
	videoId, err := uuid.Parse(videoIdString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}
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
	video, err := cfg.db.GetVideo(videoId)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not extract video by ID", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not the owner of this video", err)
		return
	}
	file, _, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()
	mediaType, _, err := mime.ParseMediaType("video/mp4")
	assetPath := getAssetPath(mediaType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not parse the media type", err)
		return
	}

	os_file, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "could not create the temp file locally", err)
		return
	}
	defer os.Remove(os_file.Name())
	defer os_file.Close() //defer is LIFO, so it will close before the remove

	io.Copy(os_file, file)

	file.Seek(0, io.SeekStart) // put the pointer back to front again
	url := cfg.getAssetURL(assetPath)
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &url,
		Body:        file,
		ContentType: &mediaType,
	})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot create a s3 PUT-OBJECT", err)
		return
	}

	s3VideoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, url)
	video.VideoURL = &s3VideoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
}
