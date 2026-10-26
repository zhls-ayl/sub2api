import { computed, ref, watch, type Ref } from 'vue'
import { DEFAULT_EXCEL_BPS_MODELS } from '@/constants/account'

// BPS OAuth 账号的模型候选：默认 BPS 模型，加上本次编辑里出现过的已选模型
// （包括「同步上游模型」追加的），取消勾选后仍留在候选里。载入账号或重新预填
// 白名单后调用 reset()，丢掉之前账号/类型留下的模型。
export function useBPSCandidateModels(selected: Ref<string[]>) {
  const seen = ref<string[]>([])

  const remember = (models: readonly string[]) => {
    const fresh = models.map(model => model.trim()).filter(model => model && !seen.value.includes(model))
    if (fresh.length > 0) {
      seen.value = [...seen.value, ...new Set(fresh)]
    }
  }

  watch(selected, models => remember(models), { deep: true })

  const reset = () => {
    seen.value = []
    remember(selected.value)
  }

  const candidates = computed(() => Array.from(new Set([...DEFAULT_EXCEL_BPS_MODELS, ...seen.value])))

  return { candidates, reset }
}
