import * as echarts from 'echarts/core'
import { BarChart, LineChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

// 报表与仪表盘页按需加载这个模块：ECharts 与图表组件不会进入首屏，
// 也不会进入入口 chunk，而是在真正需要画图时才下载。
echarts.use([
  BarChart,
  LineChart,
  GridComponent,
  LegendComponent,
  TooltipComponent,
  CanvasRenderer,
])

export default echarts
