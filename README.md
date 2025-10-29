# Sora 2 CLI Tool

An interactive Go CLI for generating videos with OpenAI's `sora-2` and `sora-2-pro` models. The tool walks you through model selection, prompt crafting, duration and resolution settings, optional reference imagery, and saving the finished MP4 locally.

## Features

- Guided, interactive prompts for every video generation setting.
- Supports both `sora-2` and `sora-2-pro` models.
- Validates clip duration and resolution inputs.
- Accepts optional image reference uploads to steer generations.
- Polls job status with progress updates until the video is ready.
- Downloads the rendered MP4 using a safe temp-file strategy.
- Loads credentials from `.env` and securely prompts for the API key when missing, with optional persistence.
- Calculates an estimated cost before submission using per-second pricing.

## Requirements

- Go 1.24 or later
- An OpenAI API key with access to the Sora video models

## Setup

```bash
git clone https://github.com/dr_sabijan/sora2-cli-tool.git
cd sora2-cli-tool
go build ./cmd/sora2cli
```

You can also run the tool without building a binary:

```bash
go run ./cmd/sora2cli
```

## Configuration

1. **Environment variables** – Set `OPENAI_API_KEY`, and optionally `OPENAI_BASE_URL`, `OPENAI_ORG_ID`, and `OPENAI_PROJECT_ID` in your shell or `.env` file.
2. **Dotenv support** – The CLI automatically reads a `.env` file in the working directory. Copy `.env.example` to `.env` and fill in your values:

```bash
cp .env.example .env
# Then edit .env with your actual API key
```

Example `.env` file:

```env
OPENAI_API_KEY=sk-...
OPENAI_BASE_URL=https://api.openai.com
```

If `OPENAI_API_KEY` is missing, the CLI prompts for it at runtime. You can opt to persist the value back into `.env` securely.

### Durations, Pricing, and Output Sizes

- Minimum clip length is **4 seconds** per Sora job.
- Pricing is estimated live in the CLI: `sora-2` is **$0.10 per second**, `sora-2-pro` is **$0.30 per second**.
- Available resolutions:
  - `sora-2`: `720x1280` (Portrait), `1280x720` (Landscape)
  - `sora-2-pro`: `720x1280`, `1280x720`, `1024x1792`, `1792x1024`
- If you leave the destination directory blank, the video is saved to the current working directory.

## Usage

Run the CLI:

```bash
./sora2cli
# or
go run ./cmd/sora2cli
```

Follow the prompts to:

- Choose the model (`sora-2` or `sora-2-pro`).
- Provide a primary prompt and optional additional description.
- Set clip duration (seconds) and output resolution (e.g., `1280x720`).
- Optionally provide a path to a reference image.
- Pick a destination directory and filename for the MP4.
- Confirm the configuration before the job is submitted.

The tool submits a generation request, polls until completion, downloads the MP4 to the location you chose, and offers to start another job immediately.

## Notes

- Ensure that the destination directory exists or can be created by the CLI.
- Existing files are not overwritten without confirmation.
- Downloaded assets expire on the OpenAI side; keep a local copy if you need long-term access.
- Respect OpenAI's usage policies and your account limits when generating videos.
