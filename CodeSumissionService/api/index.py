import os
import json
import uuid
import time
import logging
from typing import Optional, List
from fastapi import FastAPI, HTTPException, status
from pydantic import BaseModel, Field
import boto3
import redis
from dotenv import load_dotenv

load_dotenv()

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)
logger = logging.getLogger("submission-service")

app = FastAPI(title="Code Submission API")

SQS_QUEUE_URL = os.environ.get("SQS_QUEUE_URL")
AWS_REGION = os.environ.get("AWS_REGION", "us-east-1")

try:
    sqs_client = boto3.client("sqs", region_name=AWS_REGION)
    logger.info("AWS SQS Client initialized.")
except Exception as init_err:
    logger.error(f"Failed to initialize AWS SQS Client: {str(init_err)}")

REDIS_URL = os.environ.get("REDIS_URL")
if not REDIS_URL:
    raise RuntimeError("REDIS_URL environment variable is missing.")
redis_client = redis.Redis.from_url(REDIS_URL, decode_responses=True)


# --- UPDATED PYDANTIC SCHEMAS ---
class TestCaseItem(BaseModel):
    input: str
    expectedOutput: Optional[str] = ""

class SubmissionRequest(BaseModel):
    sourceCode: str = Field(..., min_length=1)
    language: str = Field(...)
    type: Optional[str] = Field(default="run", description="Job type context: 'run' or 'submit'")
    testCases: List[TestCaseItem] = Field(..., min_items=1, description="List of multiple test cases to run.")
    timeLimitMs: Optional[int] = Field(default=2000)
    memoryLimitMb: Optional[int] = Field(default=256)


@app.post("/submissions", status_code=status.HTTP_202_ACCEPTED)
async def create_submission(payload: SubmissionRequest):
    if not SQS_QUEUE_URL:
        raise HTTPException(status_code=500, detail="SQS URL missing.")

    submission_id = str(uuid.uuid4())
    
    # Unit Conversions
    time_limit_seconds = float(payload.timeLimitMs / 1000.0)
    memory_limit_kb = int(payload.memoryLimitMb * 1024)

    # Convert the list of inputs into the Go Struct's expected map[string]string layout
    go_test_cases_map = {}
    for tc in payload.testCases:
        # Keeping standard formatting constraints intact
        formatted_input = tc.input if tc.input.endswith("\n") else f"{tc.input}\n"
        go_test_cases_map[formatted_input] = tc.expectedOutput or ""

    # Using the first test case as primary 'stdin' for backward compatibility in the Go Struct
    primary_stdin = payload.testCases[0].input

    # FIX: 'type' is now fully dynamic and reads from your payload instead of hardcoded to 'run'
    job_payload = {
        "jobId": submission_id,
        "type": payload.type.lower(), 
        "sourceCode": payload.sourceCode,
        "language": payload.language.lower(),
        "stdin": primary_stdin,
        "testCases": go_test_cases_map,
        "timeLimitSeconds": time_limit_seconds,
        "memoryLimitKb": memory_limit_kb,
        "userId": "system-user",
        "queuedAt": int(time.time())
    }

    try:
        sqs_client.send_message(
            QueueUrl=SQS_QUEUE_URL,
            MessageBody=json.dumps(job_payload)
        )
        logger.info(f"Dispatched multi-test job {submission_id} with type '{payload.type}' to SQS.")
    except Exception as e:
        logger.error("Failed dispatching to AWS SQS Queue:", exc_info=True)
        raise HTTPException(status_code=500, detail="Failed to queue pipeline.")

    return {
        "submissionId": submission_id,
        "status": "QUEUED"
    }


@app.get("/submissions/{submission_id}")
async def get_submission_result(submission_id: str):
    redis_key = f"job:{submission_id}"
    try:
        result_data = redis_client.hgetall(redis_key)
    except Exception as e:
        logger.error(f"Error reading Hash from Redis:", exc_info=True)
        raise HTTPException(status_code=500, detail="Redis read fail.")

    if not result_data:
        return {
            "submissionId": submission_id, 
            "status": "QUEUED",
            "stdout": None,
            "stderr": None,
            "runtime": None,
            "memory": None
        }

    if "result" in result_data and isinstance(result_data["result"], str):
        try:
            result_data["result"] = json.loads(result_data["result"])
        except json.JSONDecodeError:
            pass

    return result_data