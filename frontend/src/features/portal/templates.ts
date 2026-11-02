import type { PortalModel } from './data';

export interface AccessTemplate {
  id: string;
  label: string;
  language: 'python' | 'bash' | 'toml';
  code: (base: string, model: string, secret: string) => string;
}
const quote = (value: string) => `'${value.replace(/'/g, `'"'"'`)}'`;
const curl =
  (path: string, body: (model: string) => unknown): AccessTemplate['code'] =>
  (base, model, secret) =>
    `curl ${quote(base + path)} \\\n  -H ${quote('Authorization: Bearer ' + secret)} \\\n  -H 'Content-Type: application/json' \\\n  -d ${quote(JSON.stringify(body(model), null, 2))}`;

export function accessTemplates(model: Pick<PortalModel, 'type' | 'apiFormats'>): AccessTemplate[] {
  const formats = model.apiFormats;
  const templates: AccessTemplate[] = [];
  if (formats.includes('typesafe/systemone') || formats.includes('commandcode/systemone'))
    templates.push({
      id: 'systemone',
      label: 'System One',
      language: 'bash',
      code: curl('/v1/systemone', (m) => ({
        model: m,
        state: 'This is a duplicate charge.',
        questions: { duplicate: { type: 'noul', instructions: 'Is this a duplicate charge?' } },
      })),
    });
  if (formats.includes('openai/decisions'))
    templates.push({
      id: 'decisions',
      label: 'Decisions',
      language: 'bash',
      code: curl('/v1/decisions', (m) => ({
        model: m,
        input: 'This is a duplicate charge.',
        questions: [{ type: 'predicate', instructions: 'Is this a duplicate charge?' }],
      })),
    });
  if (model.type === 'chat') {
    if (formats.includes('openai/chat_completions'))
      templates.push({
        id: 'chat',
        label: 'OpenAI Chat SDK',
        language: 'python',
        code: (base, model, secret) =>
          `from openai import OpenAI\n\nclient = OpenAI(api_key=${JSON.stringify(secret)}, base_url=${JSON.stringify(base + '/v1')})\nresult = client.chat.completions.create(\n    model=${JSON.stringify(model)},\n    messages=[{"role": "user", "content": "Hello"}],\n)\nprint(result.choices[0].message.content)`,
      });
    if (formats.includes('openai/responses')) {
      templates.push({
        id: 'responses',
        label: 'Responses',
        language: 'bash',
        code: curl('/v1/responses', (m) => ({ model: m, input: 'Hello' })),
      });
      templates.push({
        id: 'codex',
        label: 'Codex CLI',
        language: 'toml',
        code: (base, model) =>
          `# ~/.codex/config.toml\nmodel = ${JSON.stringify(model)}\nmodel_provider = "axonhub"\n\n[model_providers.axonhub]\nname = "AxonHub"\nbase_url = ${JSON.stringify(base + '/v1')}\nenv_key = "AXONHUB_API_KEY"\nwire_api = "responses"`,
      });
    }
    if (formats.includes('anthropic/messages'))
      templates.push({
        id: 'anthropic',
        label: 'Anthropic',
        language: 'bash',
        code: curl('/anthropic/v1/messages', (m) => ({ model: m, max_tokens: 256, messages: [{ role: 'user', content: 'Hello' }] })),
      });
    if (formats.includes('gemini/contents'))
      templates.push({
        id: 'gemini',
        label: 'Gemini',
        language: 'bash',
        code: (base, m, secret) =>
          `curl ${quote(base + '/gemini/v1beta/models/' + encodeURIComponent(m) + ':generateContent')} \\\n  -H ${quote('Authorization: Bearer ' + secret)} \\\n  -H 'Content-Type: application/json' \\\n  -d '{"contents":[{"parts":[{"text":"Hello"}]}]}'`,
      });
  }
  if (model.type === 'embedding' && formats.includes('openai/embeddings'))
    templates.push({
      id: 'embedding',
      label: 'Embeddings',
      language: 'bash',
      code: curl('/v1/embeddings', (m) => ({ model: m, input: 'Hello' })),
    });
  else if (model.type === 'embedding' && formats.includes('jina/embeddings'))
    templates.push({
      id: 'embedding',
      label: 'Jina embeddings',
      language: 'bash',
      code: curl('/jina/v1/embeddings', (m) => ({ model: m, input: 'Hello' })),
    });
  if (model.type === 'rerank' && formats.includes('jina/rerank'))
    templates.push({
      id: 'rerank',
      label: 'Rerank',
      language: 'bash',
      code: curl('/v1/rerank', (m) => ({ model: m, query: 'How to connect?', documents: ['Use your personal key.', 'Contact support.'] })),
    });
  if (model.type === 'image_generation' && formats.includes('openai/image_generation'))
    templates.push({
      id: 'image',
      label: 'Images',
      language: 'bash',
      code: curl('/v1/images/generations', (m) => ({ model: m, prompt: 'A quiet mountain lake' })),
    });
  if (model.type === 'video_generation' && formats.includes('openai/video'))
    templates.push({
      id: 'video',
      label: 'Video',
      language: 'bash',
      code: curl('/v1/videos', (m) => ({ model: m, prompt: 'A quiet mountain lake' })),
    });
  else if (model.type === 'video_generation' && formats.includes('seedance/video'))
    templates.push({
      id: 'video',
      label: 'Seedance',
      language: 'bash',
      code: curl('/doubao/v3/contents/generations/tasks', (m) => ({
        model: m,
        content: [{ type: 'text', text: 'A quiet mountain lake' }],
      })),
    });
  return templates;
}
