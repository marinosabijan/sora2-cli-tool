package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

const (
	defaultDurationSeconds = 4
	pollInterval           = 5 * time.Second
	maxWaitDuration        = 30 * time.Minute
	videosPath             = "/v1/videos"
	envFileName            = ".env"
)

type resolutionOption struct {
	Label string
	Value string
}

type modelOption struct {
	Name          string
	RatePerSecond float64
	Resolutions   []resolutionOption
}

var modelOptions = []modelOption{
	{
		Name:          "sora-2",
		RatePerSecond: 0.10,
		Resolutions: []resolutionOption{
			{Label: "Portrait (720x1280)", Value: "720x1280"},
			{Label: "Landscape (1280x720)", Value: "1280x720"},
		},
	},
	{
		Name:          "sora-2-pro",
		RatePerSecond: 0.30,
		Resolutions: []resolutionOption{
			{Label: "Portrait (720x1280)", Value: "720x1280"},
			{Label: "Landscape (1280x720)", Value: "1280x720"},
			{Label: "Portrait (1024x1792)", Value: "1024x1792"},
			{Label: "Landscape (1792x1024)", Value: "1792x1024"},
		},
	},
}

func main() {
	fmt.Println("Sora-2 Video Generator")
	fmt.Println("========================")

	envPath := resolveEnvPath()
	if err := loadEnvFile(envPath); err != nil {
		fmt.Printf("WARNING: unable to load %s: %v\n", envPath, err)
	}

	reader := bufio.NewReader(os.Stdin)

	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		fmt.Println("OPENAI_API_KEY not found in environment or .env")
		for {
			var err error
			apiKey, err = promptAPIKey()
			if err != nil {
				fmt.Printf("Input error: %v\n", err)
				continue
			}
			apiKey = strings.TrimSpace(apiKey)
			if apiKey == "" {
				fmt.Println("API key cannot be empty.")
				continue
			}
			break
		}
		if err := os.Setenv("OPENAI_API_KEY", apiKey); err != nil {
			fmt.Printf("WARNING: unable to set OPENAI_API_KEY: %v\n", err)
		}
		reader = bufio.NewReader(os.Stdin)
		if promptConfirm(reader, "Save API key to .env for future runs?") {
			if err := upsertEnvValue(envPath, "OPENAI_API_KEY", apiKey); err != nil {
				fmt.Printf("WARNING: unable to write %s: %v\n", envPath, err)
			} else {
				fmt.Printf("Saved API key to %s\n", envPath)
			}
		}
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}

	httpClient := &http.Client{Timeout: 60 * time.Second}

	for {
		model := promptModel(reader)
		primaryPrompt := promptRequired(reader, "Primary prompt")
		additionalPrompt := promptOptional(reader, "Additional description (optional)")
		combinedPrompt := primaryPrompt
		if additionalPrompt != "" {
			combinedPrompt = combinedPrompt + "\n\n" + additionalPrompt
		}

		seconds, secondsInt := promptDuration(reader, defaultDurationSeconds, defaultDurationSeconds)
		selectedResolution := promptResolutionSelection(reader, model.Resolutions)
		size := selectedResolution.Value
		referencePath := promptOptional(reader, "Path to reference image (optional)")

		var expandedReferencePath string
		if referencePath != "" {
			var err error
			expandedReferencePath, err = expandPath(referencePath)
			if err != nil {
				fmt.Printf("ERROR: %v\n", err)
				os.Exit(1)
			}
			if _, err = os.Stat(expandedReferencePath); err != nil {
				fmt.Printf("ERROR: unable to access reference file: %v\n", err)
				os.Exit(1)
			}
		}

		destinationDir := promptOptional(reader, "Destination directory for the video (leave blank to use current directory)")
		destinationDir = strings.TrimSpace(destinationDir)

		var expandedDest string
		var err error
		if destinationDir == "" {
			expandedDest, err = os.Getwd()
			if err != nil {
				fmt.Printf("ERROR: unable to determine current directory: %v\n", err)
				os.Exit(1)
			}
		} else {
			expandedDest, err = expandPath(destinationDir)
			if err != nil {
				fmt.Printf("ERROR: %v\n", err)
				os.Exit(1)
			}
			if err = os.MkdirAll(expandedDest, 0o755); err != nil {
				fmt.Printf("ERROR: unable to create destination directory: %v\n", err)
				os.Exit(1)
			}
		}

		outputName := promptRequired(reader, "Output video filename (without extension)")
		outputName = sanitizeFilename(outputName)
		if outputName == "" {
			fmt.Println("ERROR: output filename is empty after sanitization.")
			os.Exit(1)
		}
		if !strings.HasSuffix(strings.ToLower(outputName), ".mp4") {
			outputName += ".mp4"
		}
		outputPath := filepath.Join(expandedDest, outputName)

		if _, err = os.Stat(outputPath); err == nil {
			overwrite := promptConfirm(reader, fmt.Sprintf("%s already exists. Overwrite?", outputPath))
			if !overwrite {
				fmt.Println("Aborted by user.")
				return
			}
		}

		fmt.Println()
		fmt.Println("Configuration summary:")
		fmt.Printf("  Model: %s\n", model.Name)
		fmt.Printf("  Duration: %d seconds\n", secondsInt)
		fmt.Printf("  Resolution: %s\n", selectedResolution.Label)
		if expandedReferencePath != "" {
			fmt.Printf("  Reference image: %s\n", expandedReferencePath)
		}
		fmt.Printf("  Save to: %s\n", outputPath)
		estimatedCost := model.RatePerSecond * float64(secondsInt)
		fmt.Printf("  Estimated cost: $%.2f (%ds @ $%.2f/s)\n", estimatedCost, secondsInt, model.RatePerSecond)
		fmt.Printf("  Minimum duration: %d seconds\n", defaultDurationSeconds)
		fmt.Println()

		if !promptConfirm(reader, "Proceed with generation?") {
			fmt.Println("Aborted by user.")
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), maxWaitDuration)
		fmt.Println()
		fmt.Println("Submitting generation request...")

		job, err := createVideoJob(ctx, httpClient, baseURL, apiKey, combinePrompts(combinedPrompt), model.Name, seconds, size, expandedReferencePath)
		if err != nil {
			cancel()
			fmt.Printf("ERROR: failed to create video job: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Job queued with ID: %s\n", job.ID)

		job, err = waitForJobCompletion(ctx, httpClient, baseURL, apiKey, job.ID)
		if err != nil {
			cancel()
			fmt.Printf("ERROR: generation failed: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("Job completed. Downloading video...")

		if err = downloadVideoContent(ctx, httpClient, baseURL, apiKey, job.ID, outputPath); err != nil {
			cancel()
			fmt.Printf("ERROR: failed to download video: %v\n", err)
			os.Exit(1)
		}
		cancel()

		fmt.Printf("Video saved to %s\n", outputPath)

		if !promptConfirm(reader, "Generate another video?") {
			fmt.Println("Done.")
			return
		}
		fmt.Println()
	}
}

func promptModel(reader *bufio.Reader) modelOption {
	for {
		fmt.Println("Select model:")
		for i, opt := range modelOptions {
			fmt.Printf("  %d) %s ($%.2f per second)\n", i+1, opt.Name, opt.RatePerSecond)
		}
		fmt.Printf("Enter choice (1-%d): ", len(modelOptions))
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("Input error: %v\n", err)
			continue
		}
		input = strings.TrimSpace(input)
		if input == "" {
			return modelOptions[0]
		}
		if idx, convErr := strconv.Atoi(input); convErr == nil {
			if idx >= 1 && idx <= len(modelOptions) {
				return modelOptions[idx-1]
			}
		}
		for _, opt := range modelOptions {
			if strings.EqualFold(input, opt.Name) {
				return opt
			}
		}
		fmt.Println("Invalid selection, please try again.")
	}
}

func promptRequired(reader *bufio.Reader, label string) string {
	for {
		fmt.Printf("%s: ", label)
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("Input error: %v\n", err)
			continue
		}
		value := strings.TrimSpace(input)
		if value == "" {
			fmt.Println("Value required.")
			continue
		}
		return value
	}
}

func promptOptional(reader *bufio.Reader, label string) string {
	fmt.Printf("%s: ", label)
	input, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("Input error: %v\n", err)
		return ""
	}
	return strings.TrimSpace(input)
}

func promptDefault(reader *bufio.Reader, label, defaultValue string) string {
	fmt.Printf("%s [%s]: ", label, defaultValue)
	input, err := reader.ReadString('\n')
	if err != nil {
		fmt.Printf("Input error: %v\n", err)
		return defaultValue
	}
	value := strings.TrimSpace(input)
	if value == "" {
		return defaultValue
	}
	return value
}

func promptDuration(reader *bufio.Reader, defaultSeconds, minSeconds int) (string, int) {
	defaultValue := strconv.Itoa(defaultSeconds)
	fmt.Printf("Minimum duration is %d seconds.\n", minSeconds)
	for {
		value := promptDefault(reader, "Clip duration in seconds", defaultValue)
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < minSeconds {
			fmt.Printf("Please enter an integer greater than or equal to %d.\n", minSeconds)
			continue
		}
		return strconv.Itoa(seconds), seconds
	}
}

func promptResolutionSelection(reader *bufio.Reader, options []resolutionOption) resolutionOption {
	for {
		fmt.Println("Select output resolution:")
		for i, opt := range options {
			fmt.Printf("  %d) %s\n", i+1, opt.Label)
		}
		fmt.Printf("Enter choice (1-%d): ", len(options))
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("Input error: %v\n", err)
			continue
		}
		input = strings.TrimSpace(input)
		if input == "" {
			return options[0]
		}
		if idx, convErr := strconv.Atoi(input); convErr == nil {
			if idx >= 1 && idx <= len(options) {
				return options[idx-1]
			}
		}
		for _, opt := range options {
			if strings.EqualFold(input, opt.Value) || strings.EqualFold(input, opt.Label) {
				return opt
			}
		}
		fmt.Println("Invalid selection, please try again.")
	}
}

func promptConfirm(reader *bufio.Reader, label string) bool {
	for {
		fmt.Printf("%s [y/N]: ", label)
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("Input error: %v\n", err)
			continue
		}
		value := strings.ToLower(strings.TrimSpace(input))
		switch value {
		case "y", "yes":
			return true
		case "n", "no", "":
			return false
		default:
			fmt.Println("Please respond with 'y' or 'n'.")
		}
	}
}

func expandPath(path string) (string, error) {
	if path == "" {
		return path, nil
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
	}
	return path, nil
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, string(os.PathSeparator), "_")
	name = strings.ReplaceAll(name, "..", "_")
	return name
}

func promptAPIKey() (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Print("Enter OpenAI API key: ")
		keyBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(keyBytes)), nil
	}
	fmt.Print("Enter OpenAI API key: ")
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(input), nil
}

func resolveEnvPath() string {
	cwd, err := os.Getwd()
	if err != nil {
		return envFileName
	}
	return filepath.Join(cwd, envFileName)
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := parseEnvLine(line)
		if !ok || key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func parseEnvLine(line string) (string, string, bool) {
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key := strings.TrimSpace(parts[0])
	value := strings.TrimSpace(parts[1])
	value = stripQuotes(value)
	return key, value, true
}

func stripQuotes(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func upsertEnvValue(path, key, value string) error {
	var lines []string
	found := false

	if content, err := os.ReadFile(path); err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(content))
		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				lines = append(lines, line)
				continue
			}
			parsedKey, _, ok := parseEnvLine(trimmed)
			if ok && parsedKey == key {
				lines = append(lines, fmt.Sprintf("%s=%s", key, value))
				found = true
				continue
			}
			lines = append(lines, line)
		}
		if err := scanner.Err(); err != nil {
			return err
		}
	}

	if !found {
		lines = append(lines, fmt.Sprintf("%s=%s", key, value))
	}

	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func combinePrompts(prompt string) string {
	return strings.TrimSpace(prompt)
}

func createVideoJob(ctx context.Context, client *http.Client, baseURL, apiKey, prompt, model, seconds, size, referencePath string) (*videoJob, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("prompt", prompt); err != nil {
		return nil, err
	}
	if model != "" {
		if err := writer.WriteField("model", model); err != nil {
			return nil, err
		}
	}
	if seconds != "" {
		if err := writer.WriteField("seconds", seconds); err != nil {
			return nil, err
		}
	}
	if size != "" {
		if err := writer.WriteField("size", size); err != nil {
			return nil, err
		}
	}

	if referencePath != "" {
		file, err := os.Open(referencePath)
		if err != nil {
			return nil, fmt.Errorf("open reference: %w", err)
		}
		defer file.Close()
		part, err := writer.CreateFormFile("input_reference", filepath.Base(referencePath))
		if err != nil {
			return nil, err
		}
		if _, err = io.Copy(part, file); err != nil {
			return nil, fmt.Errorf("copy reference: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+videosPath, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")

	if org := strings.TrimSpace(os.Getenv("OPENAI_ORG_ID")); org != "" {
		req.Header.Set("OpenAI-Organization", org)
	}
	if project := strings.TrimSpace(os.Getenv("OPENAI_PROJECT_ID")); project != "" {
		req.Header.Set("OpenAI-Project", project)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		apiErr := readAPIError(resp.Body)
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, apiErr)
	}

	var job videoJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}
	if job.ID == "" {
		return nil, errors.New("response missing job ID")
	}
	return &job, nil
}

func waitForJobCompletion(ctx context.Context, client *http.Client, baseURL, apiKey, jobID string) (*videoJob, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastStatus string
	var lastProgress float64 = -1

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			job, err := getVideoJob(ctx, client, baseURL, apiKey, jobID)
			if err != nil {
				return nil, err
			}
			progress := normalizeProgress(job.Progress)
			if job.Status != lastStatus || progress != lastProgress {
				fmt.Printf("Status: %s (%.0f%%)\n", job.Status, progress)
				lastStatus = job.Status
				lastProgress = progress
			}

			switch strings.ToLower(job.Status) {
			case "completed":
				return job, nil
			case "failed", "canceled", "cancelled", "rejected", "expired":
				if job.Error != nil {
					return nil, fmt.Errorf("job %s: %s", job.Status, job.Error.Message)
				}
				return nil, fmt.Errorf("job %s", job.Status)
			}
		}
	}
}

func getVideoJob(ctx context.Context, client *http.Client, baseURL, apiKey, jobID string) (*videoJob, error) {
	url := fmt.Sprintf("%s%s/%s", baseURL, videosPath, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		apiErr := readAPIError(resp.Body)
		return nil, fmt.Errorf("API error (%d): %s", resp.StatusCode, apiErr)
	}

	var job videoJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

func downloadVideoContent(ctx context.Context, client *http.Client, baseURL, apiKey, jobID, outputPath string) error {
	url := fmt.Sprintf("%s%s/%s/content", baseURL, videosPath, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "video/mp4")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		apiErr := readAPIError(resp.Body)
		return fmt.Errorf("API error (%d): %s", resp.StatusCode, apiErr)
	}

	tmpPath := outputPath + ".tmp"
	outFile, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	if _, err = io.Copy(outFile, resp.Body); err != nil {
		outFile.Close()
		os.Remove(tmpPath)
		return err
	}

	if err = outFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}

	if err = os.Rename(tmpPath, outputPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func normalizeProgress(progress float64) float64 {
	if progress <= 1 && progress >= 0 {
		return progress * 100
	}
	return progress
}

func readAPIError(body io.Reader) string {
	data, err := io.ReadAll(body)
	if err != nil {
		return err.Error()
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "unknown error"
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err == nil {
		if errBlock, ok := parsed["error"].(map[string]any); ok {
			if msg, ok := errBlock["message"].(string); ok && msg != "" {
				return msg
			}
		}
	}
	return trimmed
}

type videoJob struct {
	ID                 string         `json:"id"`
	Object             string         `json:"object"`
	Model              string         `json:"model"`
	Status             string         `json:"status"`
	Progress           float64        `json:"progress"`
	CreatedAt          int64          `json:"created_at"`
	CompletedAt        int64          `json:"completed_at"`
	ExpiresAt          int64          `json:"expires_at"`
	Size               string         `json:"size"`
	Seconds            string         `json:"seconds"`
	Quality            string         `json:"quality"`
	RemixedFromVideoID string         `json:"remixed_from_video_id"`
	Error              *videoJobError `json:"error"`
}

type videoJobError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}
