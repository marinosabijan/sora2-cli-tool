package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

var (
	supportedReferenceMIMEs = []string{
		"image/jpeg",
		"image/png",
		"image/webp",
		"video/mp4",
	}
	referenceMIMECandidates = map[string]string{
		"image/jpeg":  "image/jpeg",
		"image/jpg":   "image/jpeg",
		"image/pjpeg": "image/jpeg",
		"image/png":   "image/png",
		"image/x-png": "image/png",
		"image/webp":  "image/webp",
		"video/mp4":   "video/mp4",
	}
)

type jobAction int

const (
	jobActionCreate jobAction = iota
	jobActionRemix
	jobActionList
)

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
		action := promptJobAction(reader)
		var continueLoop bool
		switch action {
		case jobActionCreate:
			continueLoop = runCreateFlow(reader, httpClient, baseURL, apiKey)
		case jobActionRemix:
			continueLoop = runRemixFlow(reader, httpClient, baseURL, apiKey)
		case jobActionList:
			continueLoop = runListFlow(reader, httpClient, baseURL, apiKey)
		default:
			continue
		}
		if !continueLoop {
			return
		}
		fmt.Println()
	}
}

func promptJobAction(reader *bufio.Reader) jobAction {
	for {
		fmt.Println("Select action:")
		fmt.Println("  1) Create a new video")
		fmt.Println("  2) Remix an existing video")
		fmt.Println("  3) List recent videos")
		fmt.Print("Enter choice (1-3): ")
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("Input error: %v\n", err)
			continue
		}
		input = strings.TrimSpace(input)
		switch strings.ToLower(input) {
		case "", "1", "create", "new", "c":
			return jobActionCreate
		case "2", "remix", "r":
			return jobActionRemix
		case "3", "list", "l":
			return jobActionList
		default:
			fmt.Println("Invalid selection, please try again.")
		}
	}
}

func runCreateFlow(reader *bufio.Reader, httpClient *http.Client, baseURL, apiKey string) bool {
	model := promptModel(reader)
	prompt := promptRequired(reader, "Prompt")

	seconds, secondsInt := promptDuration(reader, defaultDurationSeconds)
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

	expandedDest := promptDestinationDirectory(reader)

	fmt.Println()
	fmt.Println("Configuration summary:")
	fmt.Printf("  Action: Create new video\n")
	fmt.Printf("  Model: %s\n", model.Name)
	fmt.Printf("  Duration: %d seconds\n", secondsInt)
	fmt.Printf("  Resolution: %s\n", selectedResolution.Label)
	if expandedReferencePath != "" {
		fmt.Printf("  Reference image: %s\n", expandedReferencePath)
	}
	fmt.Printf("  Destination: %s (filename will match job ID)\n", expandedDest)
	estimatedCost := model.RatePerSecond * float64(secondsInt)
	fmt.Printf("  Estimated cost: $%.2f (%ds @ $%.2f/s)\n", estimatedCost, secondsInt, model.RatePerSecond)
	fmt.Println()

	if !promptConfirm(reader, "Proceed with generation?") {
		fmt.Println("Aborted by user.")
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), maxWaitDuration)
	fmt.Println()
	fmt.Println("Submitting generation request...")

	job, err := createVideoJob(ctx, httpClient, baseURL, apiKey, combinePrompts(prompt), model.Name, seconds, size, expandedReferencePath)
	if err != nil {
		cancel()
		fmt.Printf("ERROR: failed to create video job: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Job queued with ID: %s\n", job.ID)
	outputPath := filepath.Join(expandedDest, job.ID+".mp4")

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
		return false
	}
	return true
}

func runRemixFlow(reader *bufio.Reader, httpClient *http.Client, baseURL, apiKey string) bool {
	originalVideoID := promptRequired(reader, "Existing video ID to remix")
	remixPrompt := promptRequired(reader, "Remix prompt (describe the change)")
	expandedDest := promptDestinationDirectory(reader)

	fmt.Println()
	fmt.Println("Configuration summary:")
	fmt.Printf("  Action: Remix existing video\n")
	fmt.Printf("  Source video ID: %s\n", originalVideoID)
	fmt.Printf("  Remix prompt: %s\n", remixPrompt)
	fmt.Printf("  Destination: %s (filename will match job ID)\n", expandedDest)
	fmt.Println()

	if !promptConfirm(reader, "Proceed with remix generation?") {
		fmt.Println("Aborted by user.")
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), maxWaitDuration)
	fmt.Println()
	fmt.Println("Submitting remix request...")

	job, err := createRemixJob(ctx, httpClient, baseURL, apiKey, originalVideoID, combinePrompts(remixPrompt))
	if err != nil {
		cancel()
		fmt.Printf("ERROR: failed to create remix job: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Remix job queued with ID: %s\n", job.ID)
	outputPath := filepath.Join(expandedDest, job.ID+".mp4")

	job, err = waitForJobCompletion(ctx, httpClient, baseURL, apiKey, job.ID)
	if err != nil {
		cancel()
		fmt.Printf("ERROR: remix failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Remix completed. Downloading video...")

	if err = downloadVideoContent(ctx, httpClient, baseURL, apiKey, job.ID, outputPath); err != nil {
		cancel()
		fmt.Printf("ERROR: failed to download remix video: %v\n", err)
		os.Exit(1)
	}
	cancel()

	fmt.Printf("Remixed video saved to %s\n", outputPath)

	if !promptConfirm(reader, "Perform another action?") {
		fmt.Println("Done.")
		return false
	}
	return true
}

func runListFlow(reader *bufio.Reader, httpClient *http.Client, baseURL, apiKey string) bool {
	limit := 20
	for {
		input := promptOptional(reader, "Number of videos to list (1-100, leave blank for 20)")
		input = strings.TrimSpace(input)
		if input == "" {
			break
		}
		value, err := strconv.Atoi(input)
		if err != nil || value <= 0 || value > 100 {
			fmt.Println("Please enter a whole number between 1 and 100, or leave blank for 20.")
			continue
		}
		limit = value
		break
	}

	order := "desc"
	for {
		input := promptOptional(reader, "Sort order (asc/desc, leave blank for desc)")
		input = strings.TrimSpace(strings.ToLower(input))
		if input == "" {
			break
		}
		if input == "asc" || input == "desc" {
			order = input
			break
		}
		fmt.Println("Please enter 'asc', 'desc', or leave blank.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	fmt.Println()
	fmt.Println("Fetching videos...")
	list, err := listVideoJobs(ctx, httpClient, baseURL, apiKey, limit, "", order)
	if err != nil {
		fmt.Printf("ERROR: failed to list videos: %v\n", err)
		return promptConfirm(reader, "Try another action?")
	}

	if len(list.Data) == 0 {
		fmt.Println("No videos found.")
	} else {
		fmt.Println()
		fmt.Printf("Showing %d video(s):\n", len(list.Data))
		fmt.Println("----------------------------------------")
		for _, job := range list.Data {
			created := "(unknown)"
			if job.CreatedAt > 0 {
				created = time.Unix(job.CreatedAt, 0).Format(time.RFC3339)
			}
			fmt.Printf("ID: %s\n", job.ID)
			fmt.Printf("  Status: %s\n", job.Status)
			if job.Model != "" {
				fmt.Printf("  Model: %s\n", job.Model)
			}
			if job.Seconds != "" {
				fmt.Printf("  Duration: %s seconds\n", job.Seconds)
			}
			if job.Size != "" {
				fmt.Printf("  Size: %s\n", job.Size)
			}
			fmt.Printf("  Created: %s\n", created)
			progress := normalizeProgress(job.Progress)
			if progress > 0 && progress <= 100 {
				fmt.Printf("  Progress: %.0f%%\n", progress)
			}
			fmt.Println("----------------------------------------")
		}
		nextCursor := list.Next
		if nextCursor == "" {
			nextCursor = list.NextCursor
		}
		if list.HasMore || nextCursor != "" {
			fmt.Println("More videos available. Use the 'after' cursor to continue pagination.")
			if nextCursor != "" {
				fmt.Printf("Next cursor: %s\n", nextCursor)
			}
		}
	}

	if !promptConfirm(reader, "Perform another action?") {
		fmt.Println("Done.")
		return false
	}
	return true
}

func promptDestinationDirectory(reader *bufio.Reader) string {
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
		return expandedDest
	}
	expandedDest, err = expandPath(destinationDir)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	if err = os.MkdirAll(expandedDest, 0o755); err != nil {
		fmt.Printf("ERROR: unable to create destination directory: %v\n", err)
		os.Exit(1)
	}
	return expandedDest
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

func readLongLine(reader *bufio.Reader) (string, error) {
	// Check if stdin is a terminal
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		// Not a terminal, use normal read
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return "", err
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		return string(line), nil
	}

	// For terminal, temporarily disable canonical mode to allow long input
	// Save current terminal state
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		// If raw mode fails, fall back to normal read
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return "", err
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			line = line[:len(line)-1]
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		return string(line), nil
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState) // Restore terminal state

	// Read in raw mode - this bypasses terminal line buffer limits
	var result []byte
	var pending []byte
	buf := make([]byte, 8192) // Read in 8KB chunks
	for {
		n, readErr := os.Stdin.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for len(pending) > 0 {
				b := pending[0]
				// Handle Enter/Return (both \n and \r)
				if b == '\n' {
					fmt.Print("\r\n")
					return string(result), nil
				}
				if b == '\r' {
					fmt.Print("\r\n")
					return string(result), nil
				}
				// Handle Ctrl+C
				if b == 3 { // ETX
					fmt.Print("\n")
					return "", errors.New("interrupted")
				}
				// Handle backspace/delete
				if b == 127 || b == 8 { // DEL or BS
					pending = pending[1:]
					if len(result) > 0 {
						result = truncateLastRune(result)
						fmt.Print("\b \b")
					}
					continue
				}
				// Ignore other control characters except tab
				if b < 32 && b != '\t' {
					pending = pending[1:]
					continue
				}
				if !utf8.FullRune(pending) {
					break
				}
				r, size := utf8.DecodeRune(pending)
				chunk := pending[:size]
				pending = pending[size:]
				if r == utf8.RuneError && size == 1 {
					continue
				}
				fmt.Print(string(chunk))
				result = append(result, chunk...)
			}
		}
		if readErr == io.EOF {
			if len(result) > 0 {
				fmt.Print("\n")
				return string(result), nil
			}
			return "", readErr
		}
		if readErr != nil {
			if len(result) > 0 {
				fmt.Print("\n")
				return string(result), nil
			}
			return "", readErr
		}
	}
}

func truncateLastRune(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	i := len(b) - 1
	for i >= 0 && !utf8.RuneStart(b[i]) {
		i--
	}
	if i < 0 {
		return b[:0]
	}
	return b[:i]
}

func promptRequired(reader *bufio.Reader, label string) string {
	for {
		fmt.Printf("%s: ", label)
		input, err := readLongLine(reader)
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

func promptDuration(reader *bufio.Reader, defaultSeconds int) (string, int) {
	allowedSeconds := []int{4, 8, 12}
	defaultIdx := 0
	for i, sec := range allowedSeconds {
		if sec == defaultSeconds {
			defaultIdx = i
			break
		}
	}
	for {
		fmt.Println("Select clip duration:")
		for i, sec := range allowedSeconds {
			marker := ""
			if i == defaultIdx {
				marker = " (default)"
			}
			fmt.Printf("  %d) %d seconds%s\n", i+1, sec, marker)
		}
		fmt.Printf("Enter choice (1-%d): ", len(allowedSeconds))
		input, err := reader.ReadString('\n')
		if err != nil {
			fmt.Printf("Input error: %v\n", err)
			continue
		}
		input = strings.TrimSpace(input)
		if input == "" {
			seconds := allowedSeconds[defaultIdx]
			return strconv.Itoa(seconds), seconds
		}
		if idx, convErr := strconv.Atoi(input); convErr == nil {
			if idx >= 1 && idx <= len(allowedSeconds) {
				seconds := allowedSeconds[idx-1]
				return strconv.Itoa(seconds), seconds
			}
		}
		for _, sec := range allowedSeconds {
			if input == strconv.Itoa(sec) {
				return strconv.Itoa(sec), sec
			}
		}
		fmt.Println("Invalid selection, please try again.")
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
	// First, try to find .env next to the binary
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		envPath := filepath.Join(execDir, envFileName)
		if _, err := os.Stat(envPath); err == nil {
			return envPath
		}
	}
	// Fallback to current working directory
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

		mimeType, err := detectReferenceMIME(file)
		if err != nil {
			return nil, fmt.Errorf("reference file: %w", err)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind reference: %w", err)
		}

		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf("form-data; name=%q; filename=%q", "input_reference", filepath.Base(referencePath)))
		header.Set("Content-Type", mimeType)
		part, err := writer.CreatePart(header)
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

func detectReferenceMIME(file *os.File) (string, error) {
	buf := make([]byte, 512)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read reference header: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind reference header: %w", err)
	}

	if n > 0 {
		if mimeType, ok := canonicalizeReferenceMIME(http.DetectContentType(buf[:n])); ok {
			return mimeType, nil
		}
	}

	ext := strings.ToLower(filepath.Ext(file.Name()))
	if ext != "" {
		if mimeType := mime.TypeByExtension(ext); mimeType != "" {
			if canonical, ok := canonicalizeReferenceMIME(mimeType); ok {
				return canonical, nil
			}
		}
	}

	return "", fmt.Errorf("unsupported reference file type; supported types: %s", strings.Join(supportedReferenceMIMEs, ", "))
}

func canonicalizeReferenceMIME(mimeType string) (string, bool) {
	mimeType = strings.TrimSpace(strings.ToLower(mimeType))
	if mimeType == "" {
		return "", false
	}
	if idx := strings.Index(mimeType, ";"); idx != -1 {
		mimeType = mimeType[:idx]
	}
	canonical, ok := referenceMIMECandidates[mimeType]
	return canonical, ok
}

func createRemixJob(ctx context.Context, client *http.Client, baseURL, apiKey, videoID, prompt string) (*videoJob, error) {
	payload := map[string]string{"prompt": prompt}
	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(payload); err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s%s/%s/remix", baseURL, videosPath, videoID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
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

func listVideoJobs(ctx context.Context, client *http.Client, baseURL, apiKey string, limit int, after, order string) (*videoListResponse, error) {
	endpoint, err := url.Parse(baseURL + videosPath)
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if after != "" {
		query.Set("after", after)
	}
	if order != "" {
		query.Set("order", order)
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
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

	var list videoListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return &list, nil
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

type videoListResponse struct {
	Object     string     `json:"object"`
	Data       []videoJob `json:"data"`
	HasMore    bool       `json:"has_more"`
	Next       string     `json:"next"`
	NextCursor string     `json:"next_cursor"`
}
