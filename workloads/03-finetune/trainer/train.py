# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Act 3: LoRA fine-tune Gemma 2B on b-mc2/sql-create-context.

Adapted from the GKE finetune-gemma-gpu tutorial, trimmed to the demo's needs:
one L4 node, checkpoints on a GCS-FUSE mount, resume after spot preemption.
Bounded loss = one SAVE_STEPS interval (stated in the runbook).
"""
import logging
import os

from checkpoints import latest_checkpoint

log = logging.getLogger("tune-worker")


def format_example(ex):
    return (
        f"question: {ex['question']}\ncontext: {ex['context']}\n"
        f"answer: {ex['answer']}"
    )


def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    model_id = os.environ.get("MODEL_ID", "google/gemma-2b")
    output_dir = os.environ.get("CKPT_DIR", "/ckpt/gemma-sql")
    max_steps = int(os.environ.get("MAX_STEPS", "300"))
    save_steps = int(os.environ.get("SAVE_STEPS", "50"))

    import torch
    from datasets import load_dataset
    from peft import LoraConfig, get_peft_model
    from transformers import (
        AutoModelForCausalLM,
        AutoTokenizer,
        DataCollatorForLanguageModeling,
        Trainer,
        TrainingArguments,
    )

    tokenizer = AutoTokenizer.from_pretrained(model_id)
    model = AutoModelForCausalLM.from_pretrained(model_id, torch_dtype=torch.bfloat16)
    model = get_peft_model(
        model,
        LoraConfig(r=8, lora_alpha=16, lora_dropout=0.05, task_type="CAUSAL_LM",
                   target_modules=["q_proj", "k_proj", "v_proj", "o_proj"]),
    )

    ds = load_dataset("b-mc2/sql-create-context", split="train")

    def tokenize(ex):
        return tokenizer(format_example(ex), truncation=True, max_length=512)

    tokenized = ds.map(tokenize, remove_columns=ds.column_names)

    args = TrainingArguments(
        output_dir=output_dir,
        max_steps=max_steps,
        save_steps=save_steps,
        save_total_limit=2,
        per_device_train_batch_size=1,
        gradient_accumulation_steps=4,
        learning_rate=2e-4,
        bf16=True,
        logging_steps=10,
        optim="adamw_torch",
        report_to=[],
    )
    trainer = Trainer(model=model, args=args, train_dataset=tokenized,
                      data_collator=DataCollatorForLanguageModeling(tokenizer, mlm=False))

    resume = latest_checkpoint(output_dir)
    log.info("resume_from_checkpoint=%s (bounded loss: one %d-step save interval)",
             resume, save_steps)
    trainer.train(resume_from_checkpoint=resume)
    trainer.save_model(os.path.join(output_dir, "final"))
    log.info("training complete")


if __name__ == "__main__":
    main()
