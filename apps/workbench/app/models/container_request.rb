class ContainerRequest < ArvadosBase
  def work_unit(label=nil)
    ContainerWorkUnit.new(self, label)
  end
end
