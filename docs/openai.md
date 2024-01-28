# OpenAI compatibility

Ollama provides limited compatibility with the [OpenAI API](https://platform.openai.com/docs/api-reference).

## Usage

### `curl`

```
curl http://localhost:11434/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
        "model": "llama2",
        "messages": [
            {
                "role": "system",
                "content": "You are a helpful assistant."
            },
            {
                "role": "user",
                "content": "Hello!"
            }
        ]
    }'
```

### OpenAI Python Library

```python
from openai import OpenAI

client = OpenAI(
    # This is the default and can be omitted
    base_url='http://localhost:11434/v1/',
    api_key='ollama',
)

chat_completion = client.chat.completions.create(
    messages=[
        {
            'role': 'user',
            'content': 'Say this is a test',
        }
    ],
    model='llama2',
)
```

### OpenAI Javascript Library

```javascript
import OpenAI from 'openai'

const openai = new OpenAI({
  baseURL: 'http://localhost:11434/v1/',
  apiKey: 'ollama',
})

const chatCompletion = await openai.chat.completions.create({
  messages: [{ role: 'user', content: 'Say this is a test' }],
  model: 'llama2',
})
```

## Endpoints

### `/v1/chat/completions`

#### Supported fields

- [x] `model`
- [x] `messages`
- [x] `frequency_penalty`
- [x] `presence_penalty`
- [x] `response_format`
- [x] `seed`
- [x] `stop`
- [x] `stream`
- [x] `temperature`
- [x] `top_p`
- [x] `max_tokens`
- [ ] `logit_bias`
- [ ] `tools`
- [ ] `tool_choice`
- [ ] `user`
