import { Sidebar, SidebarContent, SidebarFooter, SidebarRail, useSidebar } from '@/components/ui/sidebar';
import { NavGroup } from '@/components/layout/nav-group';
import { NavUser } from '@/components/layout/nav-user';
import { MobileHeaderControls } from './mobile-header-controls';
import { SidebarData } from './types';

type IProps = React.ComponentProps<typeof Sidebar> & {
  sidebarData: SidebarData;
  showMobileControls?: boolean;
};

export function AppSidebar({ sidebarData, showMobileControls = true, ...props }: IProps) {
  const { isMobile } = useSidebar();

  return (
    <Sidebar collapsible='icon' variant='floating' {...props}>
      <SidebarContent>
        {sidebarData.navGroups.map((props) => (
          <NavGroup key={props.title} {...props} />
        ))}
      </SidebarContent>
      <SidebarFooter>
        {isMobile && showMobileControls && <MobileHeaderControls />}
        <NavUser user={sidebarData.user} />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  );
}
