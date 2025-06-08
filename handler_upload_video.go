package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func processVideoForFastStart(inputFilePath string) (string, error) {
	processedFilePath := fmt.Sprintf("%s.processing", inputFilePath)

	cmd := exec.Command("ffmpeg", "-i", inputFilePath, "-movflags", "faststart", "-codec", "copy", "-f", "mp4", processedFilePath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(processedFilePath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return processedFilePath, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	err := cmd.Run()
	if err != nil {
		fmt.Println(err)
		return "Unable to execute the command", err
	}

	type streamDetails struct {
		Streams []struct {
			DisplayAspectRatio string `json:"display_aspect_ratio,omitempty"`
		} `json:"streams"`
	}
	params := streamDetails{}
	err = json.Unmarshal(buf.Bytes(), &params)
	if err != nil {
		return "", err
	}
	video_aspect_ratio := params.Streams[0].DisplayAspectRatio
	if video_aspect_ratio == "16:9" || video_aspect_ratio == "9:16" {
		return video_aspect_ratio, nil
	}
	return "other", nil

}

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
	// 3.1 Checking if the video Owner in the DB is the same as the User obtained from JWT
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

	//6.1 Get the Video prefix("potrait", "landscape", "other")
	video_prefix := ""
	aspectRatio, err := getVideoAspectRatio(os_file.Name())

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error determining aspect ratio", err)
		return
	}
	switch aspectRatio {
	case "16:9":
		video_prefix = "landscape"
	case "9:16":
		video_prefix = "portrait"
	default:
		video_prefix = "other"
	}

	// 6.2 Process the video

	// 7. put the <os_file> into S3 bucket..add the prefix to the path

	key := video_prefix + "/" + getAssetPath(mediaType)
	processedFilePath, err := processVideoForFastStart(os_file.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video", err)
		return
	}
	defer os.Remove(processedFilePath)

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not open processed file", err)
		return
	}
	defer processedFile.Close()
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error uploading file to S3", err)
		return
	}

	// 8. store video_URL as <bucket_name>,<key>
	video.VideoURL = aws.String((fmt.Sprintf("%s,%s", cfg.s3Bucket, key)))
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate presigned URL", err)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	client := s3.NewPresignClient(s3Client)
	req, err := client.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %v", err)
	}
	return req.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}
	str_slice := strings.Split(*video.VideoURL, ",")
	if len(str_slice) < 2 {
		return video, nil
	}
	s3Bucket, s3Key := str_slice[0], str_slice[1]

	signedURL, err := generatePresignedURL(cfg.s3Client, s3Bucket, s3Key, 5*time.Minute)
	if err != nil {
		return video, err
	}
	video.VideoURL = &(signedURL)
	return video, nil
}
