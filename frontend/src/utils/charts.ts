import * as echarts from 'echarts/core'
import { BarChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

// 报表页按需加载这个模块：ECharts 与只在柱状图用到的组件不会进入首屏，
// 也不会进入报表页的入口 chunk，而是在真正需要画图时才下载。
echarts.use([BarChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

export default echarts
